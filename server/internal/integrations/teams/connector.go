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
	// legacy marks bots whose Connector only accepts tokens minted by the
	// shared "botframework.com" authority — multi-tenant registrations made
	// before single-tenant became the default (Teams Developer Portal bots
	// created before late 2025, older Azure Bots). Learned on the first 401.
	legacy map[string]bool
}

// legacyAuthority is the token authority multi-tenant bots use.
const legacyAuthority = "botframework.com"

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
		legacy:    make(map[string]bool),
	}
}

func appKey(creds credentials) string { return creds.TenantID + "|" + creds.AppID }

// authority returns the tenant the bot's tokens are minted in.
func (s *tokenSource) authority(creds credentials) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legacy[appKey(creds)] {
		return legacyAuthority
	}
	return creds.TenantID
}

// fallBackToLegacy switches a bot to the multi-tenant authority after the
// Connector refused its tenant-issued token. Reports false when the bot was
// already on it (nothing left to try).
func (s *tokenSource) fallBackToLegacy(creds credentials) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := appKey(creds)
	if s.legacy[k] {
		return false
	}
	s.legacy[k] = true
	return true
}

// Token returns a valid bearer token for creds, minting one when the cache is
// empty or about to expire. The cache key includes a hash-free secret
// fingerprint (its length and tail) so a rotated secret does not keep using a
// token minted by the old one past the rotation — it is minted again.
func (s *tokenSource) Token(ctx context.Context, creds credentials) (string, error) {
	authority := s.authority(creds)
	key := authority + "|" + creds.AppID + "|" + secretFingerprint(creds.AppSecret)
	s.mu.Lock()
	if t, ok := s.tokens[key]; ok && s.now().Add(tokenRefreshMargin).Before(t.expiresAt) {
		s.mu.Unlock()
		return t.value, nil
	}
	s.mu.Unlock()

	tok, err := s.mintFrom(ctx, creds, authority)
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

// mint obtains a token from the bot's own tenant (install-time check).
func (s *tokenSource) mint(ctx context.Context, creds credentials) (cachedToken, error) {
	return s.mintFrom(ctx, creds, creds.TenantID)
}

func (s *tokenSource) mintFrom(ctx context.Context, creds credentials, authority string) (cachedToken, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", creds.AppID)
	form.Set("client_secret", creds.AppSecret)
	form.Set("scope", connectorScope)
	endpoint := s.loginBase + "/" + url.PathEscape(authority) + "/oauth2/v2.0/token"

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
	Attachments  []outAttachment   `json:"attachments,omitempty"`
}

// outAttachment is a message attachment the bot sends (an inline picture).
type outAttachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name,omitempty"`
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

// createConversationRequest is the ConversationParameters subset needed to
// open (or reopen) the bot's personal chat with one user.
type createConversationRequest struct {
	Bot         channelAccountRef   `json:"bot"`
	Members     []channelAccountRef `json:"members"`
	IsGroup     bool                `json:"isGroup"`
	TenantID    string              `json:"tenantId"`
	ChannelData map[string]any      `json:"channelData"`
}

type channelAccountRef struct {
	ID string `json:"id"`
}

// CreatePersonalConversation returns the id of the bot's 1:1 chat with
// userID, creating it when needed. Teams answers with the existing
// conversation for a user who already chats with the bot, so the call is
// idempotent and is how proactive messages find a user's personal chat.
func (c *connector) CreatePersonalConversation(ctx context.Context, serviceURL, userID string) (string, error) {
	req := createConversationRequest{
		Bot:         channelAccountRef{ID: botIDPrefix + c.creds.AppID},
		Members:     []channelAccountRef{{ID: userID}},
		TenantID:    c.creds.TenantID,
		ChannelData: map[string]any{"tenant": map[string]string{"id": c.creds.TenantID}},
	}
	var rr resourceResponse
	if err := c.do(ctx, http.MethodPost, serviceURL, "/v3/conversations", req, &rr); err != nil {
		return "", err
	}
	if rr.ID == "" {
		return "", errors.New("teams connector: create conversation returned no id")
	}
	return rr.ID, nil
}

// SendAttachments posts a message carrying attachments (text may be empty).
func (c *connector) SendAttachments(ctx context.Context, serviceURL, conversationID, text string, atts []outAttachment) (string, error) {
	act := outActivity{Type: "message", Text: text, Attachments: atts}
	if text != "" {
		act.TextFormat = "markdown"
	}
	path := "/v3/conversations/" + url.PathEscape(conversationID) + "/activities"
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
	UserRole          string `json:"userRole"` // "user" | "guest" | "anonymous"
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
	var payload []byte
	if in != nil {
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		status, raw, err := c.send(ctx, method, base+path, payload)
		if err != nil {
			return fmt.Errorf("teams connector %s %s: %w", method, path, err)
		}
		if status == http.StatusUnauthorized && attempt == 0 && c.tokens.fallBackToLegacy(c.creds) {
			// A multi-tenant registration rejects tenant-issued tokens; try
			// the shared authority once and remember the answer.
			continue
		}
		if status < 200 || status > 299 {
			return &apiError{Status: status, Body: truncate(string(raw), maxErrorBody)}
		}
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("teams connector: decode response: %w", err)
			}
		}
		return nil
	}
}

func (c *connector) send(ctx context.Context, method, url string, payload []byte) (int, []byte, error) {
	token, err := c.tokens.Token(ctx, c.creds)
	if err != nil {
		return 0, nil, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// validServiceURL guards the one URL we take from inbound traffic. The
// serviceUrl is covered by the Connector's signed token (webhook.go checks the
// claim), and is persisted on the session binding; this check is defence in
// depth so a corrupted binding row can never point the bot's bearer token at
// an arbitrary host.
func validServiceURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && allowInsecureServiceURLForTests && u.Scheme == "http" && u.Host != "" {
		return strings.TrimRight(u.String(), "/"), nil
	}
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil {
		return "", fmt.Errorf("teams: invalid serviceUrl %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if !trustedServiceHosts[host] {
		return "", fmt.Errorf("teams: serviceUrl host %q is not a Teams Connector host", host)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// trustedServiceHosts are the exact Connector hosts Teams uses in the public
// commercial cloud and GCC — the clouds whose tokens auth.go accepts (iss
// https://api.botframework.com). Exact matches only: a suffix such as
// ".trafficmanager.net" is a namespace any Azure customer can register in.
var trustedServiceHosts = map[string]bool{
	"smba.trafficmanager.net":            true,
	"smba.infra.gcc.teams.microsoft.com": true,
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
