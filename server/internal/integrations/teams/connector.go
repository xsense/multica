package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is the outbound half of the Bot Framework transport: acquiring
// the bot's app-only token from Entra and calling the Connector REST API
// (send / update activity, typing, member lookup).
//
// References:
//   - https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication
//   - https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference

const (
	// defaultLoginBase is the Entra authority host. Single-tenant bots
	// request tokens from their own tenant: <base>/<tenant>/oauth2/v2.0/token.
	defaultLoginBase = "https://login.microsoftonline.com"
	// connectorScope is the resource every Connector call is authorized for.
	connectorScope = "https://api.botframework.com/.default"
	// tokenRefreshMargin renews a cached token this long before it expires.
	tokenRefreshMargin = 5 * time.Minute
	// connectorTimeout bounds one Connector call.
	connectorTimeout = 20 * time.Second
	// maxErrorBody caps how much of an error body is kept for logs.
	maxErrorBody = 2048
)

var (
	// ErrTokenRejected means Entra refused the client credentials
	// (invalid_client / unauthorized_client / unknown tenant). The admin has
	// to fix the App ID, tenant or secret; retrying cannot help.
	ErrTokenRejected = errors.New("teams: Microsoft Entra rejected the bot credentials")
)

// apiError is a non-2xx Connector or token-endpoint response.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("teams api: HTTP %d: %s", e.Status, e.Body)
}

// isRetryable reports whether a Connector failure is worth one more attempt:
// throttling and server-side errors. 4xx other than 429 are definite.
func isRetryable(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	return true // transport error
}

// tokenSource acquires and caches app-only tokens per (tenant, app id). It is
// shared by every connector client in the process so N concurrent replies do
// not each mint a token.
type tokenSource struct {
	loginBase string
	client    *http.Client
	now       func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

func newTokenSource(loginBase string, client *http.Client) *tokenSource {
	if loginBase == "" {
		loginBase = defaultLoginBase
	}
	if client == nil {
		client = &http.Client{Timeout: connectorTimeout}
	}
	return &tokenSource{
		loginBase: strings.TrimRight(loginBase, "/"),
		client:    client,
		now:       time.Now,
		tokens:    make(map[string]cachedToken),
	}
}

// Token returns a valid bearer token for creds, minting one when the cache is
// empty or about to expire. The cache key includes a hash-free secret
// fingerprint (its length and tail) so a rotated secret does not keep using a
// token minted by the old one past the rotation — it is minted again.
func (s *tokenSource) Token(ctx context.Context, creds credentials) (string, error) {
	key := creds.TenantID + "|" + creds.AppID + "|" + secretFingerprint(creds.AppSecret)
	s.mu.Lock()
	if t, ok := s.tokens[key]; ok && s.now().Add(tokenRefreshMargin).Before(t.expiresAt) {
		s.mu.Unlock()
		return t.value, nil
	}
	s.mu.Unlock()

	tok, err := s.mint(ctx, creds)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[key] = tok
	s.mu.Unlock()
	return tok.value, nil
}

func secretFingerprint(secret string) string {
	if len(secret) <= 4 {
		return fmt.Sprintf("%d", len(secret))
	}
	return fmt.Sprintf("%d:%s", len(secret), secret[len(secret)-4:])
}

func (s *tokenSource) mint(ctx context.Context, creds credentials) (cachedToken, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", creds.AppID)
	form.Set("client_secret", creds.AppSecret)
	form.Set("scope", connectorScope)
	endpoint := s.loginBase + "/" + url.PathEscape(creds.TenantID) + "/oauth2/v2.0/token"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return cachedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("teams token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		switch oauthErr.Error {
		case "invalid_client", "unauthorized_client", "invalid_request", "invalid_resource":
			// Entra answers 400/401 with one of these for a wrong secret, an
			// unknown App ID or an App ID that does not live in this tenant.
			return cachedToken{}, fmt.Errorf("%w: %s", ErrTokenRejected, firstLine(oauthErr.Description))
		}
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(oauthErr.Description, "AADSTS90002") {
			// Tenant not found.
			return cachedToken{}, fmt.Errorf("%w: %s", ErrTokenRejected, firstLine(oauthErr.Description))
		}
		return cachedToken{}, &apiError{Status: resp.StatusCode, Body: truncate(string(body), maxErrorBody)}
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return cachedToken{}, fmt.Errorf("teams token response: missing access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	return cachedToken{
		value:     tr.AccessToken,
		expiresAt: s.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// ---- Connector REST client ----

// connector is one installation's Connector client. It is cheap to build per
// call: the token cache lives in the shared tokenSource.
type connector struct {
	creds  credentials
	tokens *tokenSource
	client *http.Client
}

func newConnector(creds credentials, tokens *tokenSource, client *http.Client) *connector {
	if client == nil {
		client = &http.Client{Timeout: connectorTimeout}
	}
	return &connector{creds: creds, tokens: tokens, client: client}
}

// outActivity is the subset of the Activity schema the bot sends.
type outActivity struct {
	Type         string            `json:"type"`
	ID           string            `json:"id,omitempty"`
	Text         string            `json:"text,omitempty"`
	TextFormat   string            `json:"textFormat,omitempty"`
	ReplyToID    string            `json:"replyToId,omitempty"`
	Conversation *conversationRef  `json:"conversation,omitempty"`
	Entities     []json.RawMessage `json:"entities,omitempty"`
}

type conversationRef struct {
	ID string `json:"id"`
}

type resourceResponse struct {
	ID string `json:"id"`
}

// SendMessage posts a new message into the conversation and returns the
// activity id Teams assigned to it. replyToID, when set, threads the message
// under that activity (the "reply to activity" endpoint); in a channel the
// conversation id already pins the thread, so it is used only to quote.
func (c *connector) SendMessage(ctx context.Context, serviceURL, conversationID, text, replyToID string) (string, error) {
	act := outActivity{Type: "message", Text: text, TextFormat: "markdown"}
	path := "/v3/conversations/" + url.PathEscape(conversationID) + "/activities"
	if replyToID != "" {
		act.ReplyToID = replyToID
		path += "/" + url.PathEscape(replyToID)
	}
	var rr resourceResponse
	if err := c.do(ctx, http.MethodPost, serviceURL, path, act, &rr); err != nil {
		return "", err
	}
	return rr.ID, nil
}

// UpdateMessage replaces the text of a message the bot sent earlier.
func (c *connector) UpdateMessage(ctx context.Context, serviceURL, conversationID, activityID, text string) error {
	act := outActivity{Type: "message", ID: activityID, Text: text, TextFormat: "markdown"}
	path := "/v3/conversations/" + url.PathEscape(conversationID) + "/activities/" + url.PathEscape(activityID)
	return c.do(ctx, http.MethodPut, serviceURL, path, act, nil)
}

// SendTyping shows the "… is typing" indicator. Teams clears it after a few
// seconds or when the bot's next message lands.
func (c *connector) SendTyping(ctx context.Context, serviceURL, conversationID string) error {
	path := "/v3/conversations/" + url.PathEscape(conversationID) + "/activities"
	return c.do(ctx, http.MethodPost, serviceURL, path, outActivity{Type: "typing"}, nil)
}

// teamsMember is the TeamsChannelAccount shape the member endpoint returns.
type teamsMember struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	AADObjectID       string `json:"aadObjectId"`
	Email             string `json:"email"`
	UserPrincipalName string `json:"userPrincipalName"`
	TenantID          string `json:"tenantId"`
}

// GetMember looks up one conversation member. Teams fills email and
// userPrincipalName here even though the inbound activity never carries them.
func (c *connector) GetMember(ctx context.Context, serviceURL, conversationID, memberID string) (teamsMember, error) {
	path := "/v3/conversations/" + url.PathEscape(conversationID) + "/members/" + url.PathEscape(memberID)
	var m teamsMember
	if err := c.do(ctx, http.MethodGet, serviceURL, path, nil, &m); err != nil {
		return teamsMember{}, err
	}
	return m, nil
}

func (c *connector) do(ctx context.Context, method, serviceURL, path string, in, out any) error {
	base, err := validServiceURL(serviceURL)
	if err != nil {
		return err
	}
	token, err := c.tokens.Token(ctx, c.creds)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("teams connector %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Status: resp.StatusCode, Body: truncate(string(raw), maxErrorBody)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("teams connector: decode response: %w", err)
		}
	}
	return nil
}

// validServiceURL guards the one URL we take from inbound traffic. The
// serviceUrl is covered by the Connector's signed token (webhook.go checks the
// claim), and is persisted on the session binding; this check is defence in
// depth so a corrupted binding row can never point the bot's bearer token at
// an arbitrary host.
func validServiceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		if allowInsecureServiceURLForTests && err == nil && u.Scheme == "http" && u.Host != "" {
			return strings.TrimRight(raw, "/"), nil
		}
		return "", fmt.Errorf("teams: invalid serviceUrl %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if !trustedServiceHost(host) {
		return "", fmt.Errorf("teams: serviceUrl host %q is not a Bot Framework host", host)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// trustedServiceHost lists the Connector hosts Teams uses. smba.* serves the
// public cloud (regional paths under smba.trafficmanager.net); the gov clouds
// use their own smba hosts.
func trustedServiceHost(host string) bool {
	switch {
	case host == "smba.trafficmanager.net",
		strings.HasSuffix(host, ".trafficmanager.net") && strings.HasPrefix(host, "smba"),
		strings.HasSuffix(host, ".botframework.com"),
		strings.HasSuffix(host, ".botframework.us"),
		strings.HasSuffix(host, ".botframework.azure.us"),
		strings.HasSuffix(host, ".infra.gov.teams.microsoft.us"),
		strings.HasSuffix(host, ".gov.teams.microsoft.us"),
		strings.HasSuffix(host, ".teams.microsoft.com"):
		return true
	}
	return false
}

// allowInsecureServiceURLForTests lets httptest servers stand in for the
// Connector. Only ever set from _test.go files.
var allowInsecureServiceURLForTests = false

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
