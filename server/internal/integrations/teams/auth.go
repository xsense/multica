package teams

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// This file validates the JWT the Bot Framework Connector attaches to every
// activity it POSTs to the messaging endpoint, per
// https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication
// ("Connector to your bot"):
//
//  1. Authorization: Bearer <jwt>
//  2. iss == https://api.botframework.com
//  3. aud == the bot's Microsoft App ID
//  4. within its validity window, allowing 5 minutes of clock skew
//  5. RS256 signature by a key from the Bot Framework OpenID JWKS
//  6. that key is endorsed for the activity's channelId ("msteams")
//  7. the serviceUrl claim equals the activity's serviceUrl
//
// There is deliberately no switch to turn any of this off.

const (
	botFrameworkOpenIDURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	botFrameworkIssuer    = "https://api.botframework.com"
	jwksRefreshInterval   = 12 * time.Hour
	// jwksMinRefetch rate-limits refetches triggered by an unknown kid, so a
	// stream of forged tokens cannot turn into a stream of outbound fetches.
	jwksMinRefetch = 5 * time.Minute
	clockSkew      = 5 * time.Minute
)

var (
	// ErrUnauthenticated covers every reason an inbound request is not
	// provably from the Connector for this bot. The handler answers 401/403
	// without detail.
	ErrUnauthenticated = errors.New("teams: request is not authenticated by the Bot Framework")
)

// signingKey is one JWKS entry the Connector may sign with.
type signingKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// keySet fetches and caches the Bot Framework signing keys.
//
// Refetches are driven by inbound traffic (an unknown kid, or a stale set),
// and inbound traffic is unauthenticated until the key is known, so the
// refetch path is hardened against being steered by it:
//   - at most one fetch per jwksMinRefetch, whatever triggered it;
//   - the fetch runs on a detached context, so a caller that drops its
//     connection cannot cancel it and burn the attempt;
//   - the data lock is never held across network I/O; concurrent callers
//     share one in-flight fetch.
type keySet struct {
	openIDURL string
	client    *http.Client
	now       func() time.Time

	mu        sync.Mutex
	keys      map[string]signingKey
	fetchedAt time.Time
	lastTry   time.Time
	inflight  chan struct{} // closed when the running fetch finishes
}

func newKeySet(openIDURL string, client *http.Client) *keySet {
	if openIDURL == "" {
		openIDURL = botFrameworkOpenIDURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &keySet{openIDURL: openIDURL, client: client, now: time.Now}
}

// key returns the signing key for kid, refreshing the cache when it is stale
// or does not know kid yet (Microsoft adds keys without notice).
func (k *keySet) key(ctx context.Context, kid string) (signingKey, error) {
	for attempt := 0; attempt < 2; attempt++ {
		k.mu.Lock()
		sk, known := k.keys[kid]
		now := k.now()
		stale := k.keys == nil || now.Sub(k.fetchedAt) > jwksRefreshInterval
		needFetch := (stale || !known) && (k.keys == nil || now.Sub(k.lastTry) > jwksMinRefetch)
		wait := k.inflight
		if wait == nil && needFetch {
			k.lastTry = now
			wait = make(chan struct{})
			k.inflight = wait
			go k.refresh(wait)
		}
		k.mu.Unlock()

		if known && !stale {
			return sk, nil
		}
		if wait == nil {
			// Nothing in flight and refetching is rate-limited: answer from
			// the cache (a stale-but-known key keeps working through a
			// metadata outage).
			if known {
				return sk, nil
			}
			return signingKey{}, fmt.Errorf("%w: unknown signing key", ErrUnauthenticated)
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return signingKey{}, fmt.Errorf("%w: %v", ErrUnauthenticated, ctx.Err())
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if sk, ok := k.keys[kid]; ok {
		return sk, nil
	}
	return signingKey{}, fmt.Errorf("%w: unknown signing key", ErrUnauthenticated)
}

// refresh fetches the key set on a detached context and publishes it.
func (k *keySet) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	keys, err := k.fetch(ctx)
	k.mu.Lock()
	if err == nil {
		k.keys = keys
		k.fetchedAt = k.now()
	} else if k.keys == nil {
		// Nothing cached yet: allow the next request to try again at once
		// rather than rejecting everything for jwksMinRefetch.
		k.lastTry = time.Time{}
	}
	k.inflight = nil
	k.mu.Unlock()
	close(done)
}

func (k *keySet) fetch(ctx context.Context) (map[string]signingKey, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := k.getJSON(ctx, k.openIDURL, &meta); err != nil {
		return nil, fmt.Errorf("teams: fetch openid metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("teams: openid metadata has no jwks_uri")
	}
	var doc struct {
		Keys []struct {
			Kty          string   `json:"kty"`
			Kid          string   `json:"kid"`
			N            string   `json:"n"`
			E            string   `json:"e"`
			Endorsements []string `json:"endorsements"`
		} `json:"keys"`
	}
	if err := k.getJSON(ctx, meta.JWKSURI, &doc); err != nil {
		return nil, fmt.Errorf("teams: fetch jwks: %w", err)
	}
	out := make(map[string]signingKey, len(doc.Keys))
	for _, jk := range doc.Keys {
		if jk.Kty != "RSA" || jk.Kid == "" {
			continue
		}
		pub, err := rsaKeyFromJWK(jk.N, jk.E)
		if err != nil {
			continue
		}
		out[jk.Kid] = signingKey{pub: pub, endorsements: jk.Endorsements}
	}
	if len(out) == 0 {
		return nil, errors.New("teams: jwks has no usable RSA keys")
	}
	return out, nil
}

func (k *keySet) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func rsaKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(nB64, "="))
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(eB64, "="))
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31 {
		return nil, errors.New("bad exponent")
	}
	n := new(big.Int).SetBytes(nb)
	if n.BitLen() < 2048 {
		return nil, errors.New("rsa key too small")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// connectorClaims are the claims the Connector token carries that we check.
type connectorClaims struct {
	ServiceURL string `json:"serviceurl"`
	jwt.RegisteredClaims
}

// Authenticator verifies inbound Connector requests.
type Authenticator struct {
	keys *keySet
	now  func() time.Time
}

// NewAuthenticator builds an Authenticator against the public Bot Framework
// metadata. openIDURL/client overrides exist for tests.
func NewAuthenticator(openIDURL string, client *http.Client) *Authenticator {
	return &Authenticator{keys: newKeySet(openIDURL, client), now: time.Now}
}

// Verify checks the Authorization header against appID and the activity's
// channelId / serviceUrl. Any failure returns an error wrapping
// ErrUnauthenticated.
func (a *Authenticator) Verify(ctx context.Context, authHeader, appID, channelID, serviceURL string) error {
	scheme, raw, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: missing bearer token", ErrUnauthenticated)
	}
	raw = strings.TrimSpace(raw)

	var endorsements []string
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(botFrameworkIssuer),
		jwt.WithAudience(appID),
		jwt.WithLeeway(clockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(a.now),
	)
	claims := &connectorClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("%w: token has no kid", ErrUnauthenticated)
		}
		sk, err := a.keys.key(ctx, kid)
		if err != nil {
			return nil, err
		}
		endorsements = sk.endorsements
		return sk.pub, nil
	})
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if !containsFold(endorsements, channelID) {
		return fmt.Errorf("%w: signing key not endorsed for channel %q", ErrUnauthenticated, channelID)
	}
	// The Connector signs the serviceUrl it will accept our replies on. A
	// mismatch means the activity body was not the one this token was minted
	// for — never reply to an unsigned URL with the bot's credentials.
	if claims.ServiceURL == "" || !sameServiceURL(claims.ServiceURL, serviceURL) {
		return fmt.Errorf("%w: serviceUrl claim does not match the activity", ErrUnauthenticated)
	}
	return nil
}

func sameServiceURL(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return norm(a) == norm(b)
}

func containsFold(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
