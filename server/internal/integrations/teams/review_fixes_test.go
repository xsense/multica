package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- identity: email auto-binding is opt-in, domain-limited, no guests ----

type fakeIdentityQueries struct {
	users    map[string]db.User
	members  map[[16]byte]bool
	bindings map[string]pgtype.UUID // channel user id → multica user
	created  []db.CreateChannelUserBindingParams
}

func (q *fakeIdentityQueries) GetChannelUserBindingByUserID(_ context.Context, arg db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error) {
	if u, ok := q.bindings[arg.ChannelUserID]; ok {
		return db.ChannelUserBinding{MulticaUserID: u}, nil
	}
	return db.ChannelUserBinding{}, pgx.ErrNoRows
}

func (q *fakeIdentityQueries) GetMemberByUserAndWorkspace(_ context.Context, arg db.GetMemberByUserAndWorkspaceParams) (db.Member, error) {
	if q.members[arg.UserID.Bytes] {
		return db.Member{}, nil
	}
	return db.Member{}, pgx.ErrNoRows
}

func (q *fakeIdentityQueries) GetUserByEmail(_ context.Context, email string) (db.User, error) {
	if u, ok := q.users[email]; ok {
		return u, nil
	}
	return db.User{}, pgx.ErrNoRows
}

func (q *fakeIdentityQueries) CreateChannelUserBinding(_ context.Context, arg db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error) {
	q.created = append(q.created, arg)
	return db.ChannelUserBinding{}, nil
}

func uuidOf(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[15] = b
	u.Valid = true
	return u
}

func identitySetup(t *testing.T, member teamsMember, domains []string) (*identityResolver, *fakeIdentityQueries, engine.ResolvedInstallation, channel.InboundMessage) {
	t.Helper()
	f := newFakeBotFramework(t)
	f.member = member
	alice := uuidOf(1)
	q := &fakeIdentityQueries{
		users:    map[string]db.User{"alice@xsense.co.th": {ID: alice}, "alice@partner.com": {ID: alice}},
		members:  map[[16]byte]bool{alice.Bytes: true},
		bindings: map[string]pgtype.UUID{},
	}
	r := &identityResolver{q: q, transport: f.transport(), domains: normalizeDomains(domains), logger: discardLogger()}
	inst := engine.ResolvedInstallation{ID: uuidOf(9), WorkspaceID: uuidOf(8), Platform: db.ChannelInstallation{Config: f.installConfig()}}
	msg, ok := inboundFromActivity(baseActivity(f.server.URL), testAppID, testTenantID)
	if !ok {
		t.Fatal("fixture message dropped")
	}
	return r, q, inst, msg
}

func TestAutoBindOffByDefault(t *testing.T) {
	r, q, inst, msg := identitySetup(t, teamsMember{Email: "alice@xsense.co.th", UserRole: "user", AADObjectID: "0f0f0f0f-0000-0000-0000-000000000001"}, nil)
	if _, err := r.ResolveSender(context.Background(), inst, msg); err != engine.ErrSenderUnbound {
		t.Fatalf("want ErrSenderUnbound, got %v", err)
	}
	if len(q.created) != 0 {
		t.Fatal("no binding may be created when auto-bind is off")
	}
}

func TestAutoBindAllowlistedDomain(t *testing.T) {
	r, q, inst, msg := identitySetup(t, teamsMember{Email: "Alice@XSense.co.th", UserRole: "user", AADObjectID: "0f0f0f0f-0000-0000-0000-000000000001"}, []string{"@xsense.co.th"})
	id, err := r.ResolveSender(context.Background(), inst, msg)
	if err != nil || id.UserID != uuidOf(1) || len(q.created) != 1 {
		t.Fatalf("expected bind to alice: id=%v err=%v created=%d", id, err, len(q.created))
	}
}

func TestAutoBindRefusesGuestsAndForeignDomains(t *testing.T) {
	cases := map[string]teamsMember{
		"guest":          {Email: "alice@xsense.co.th", UserRole: "guest", AADObjectID: "0f0f0f0f-0000-0000-0000-000000000001"},
		"foreign domain": {Email: "alice@partner.com", UserRole: "user", AADObjectID: "0f0f0f0f-0000-0000-0000-000000000001"},
		"other object":   {Email: "alice@xsense.co.th", UserRole: "user", AADObjectID: "99999999-0000-0000-0000-000000000001"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			r, q, inst, msg := identitySetup(t, m, []string{"xsense.co.th"})
			if _, err := r.ResolveSender(context.Background(), inst, msg); err != engine.ErrSenderUnbound {
				t.Fatalf("want ErrSenderUnbound, got %v", err)
			}
			if len(q.created) != 0 {
				t.Fatal("binding must not be created")
			}
		})
	}
}

// ---- typing: a late-ending loop never stops a newer one ----

func TestTypingReleaseKeepsNewerLoop(t *testing.T) {
	m := NewTypingManager(nil, discardLogger())
	_, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	a := &typingLoop{cancel: cancelA}
	b := &typingLoop{cancel: cancelB}
	m.active["s"] = a
	m.stop("s")       // run A ended
	m.active["s"] = b // next message started B
	m.release("s", a) // A's deferred exit lands late
	if m.active["s"] != b || ctxB.Err() != nil {
		t.Fatal("A's release must not remove or cancel B")
	}
}

// ---- key set: refetch is rate-limited and survives caller cancellation ----

func TestKeySetRefetchIsRateLimitedAndDetached(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var fetches atomic.Int32
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "real",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	ks := newKeySet(srv.URL+"/openid", srv.Client())

	// A caller that gives up immediately must not cancel the fetch.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = ks.key(gone, "real")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ks.mu.Lock()
		loaded := ks.keys != nil
		ks.mu.Unlock()
		if loaded || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := ks.key(context.Background(), "real"); err != nil {
		t.Fatalf("detached fetch should have loaded the key: %v", err)
	}
	before := fetches.Load()
	for i := 0; i < 20; i++ {
		_, _ = ks.key(context.Background(), "forged-kid")
	}
	if got := fetches.Load() - before; got != 0 {
		t.Fatalf("unknown kids within the refetch window caused %d fetches", got)
	}
}

// ---- text cleanup ----

func TestPlainTextKeepsAngleBracketsAndBotNameInBody(t *testing.T) {
	a := baseActivity("https://x")
	a.TextFormat = "plain"
	a.Text = "@Helper is a<b>c true? Ask Helper later"
	got := cleanText(a, testAppID)
	if got != "is a<b>c true? Ask Helper later" {
		t.Fatalf("got %q", got)
	}
}

// ---- legacy multi-tenant bots: fall back to the botframework.com authority ----

func TestConnectorFallsBackToLegacyAuthorityOn401(t *testing.T) {
	var legacyMints atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/"+testTenantID+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tenant-token", "expires_in": 3600})
	})
	mux.HandleFunc("/botframework.com/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		legacyMints.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "legacy-token", "expires_in": 3600})
	})
	mux.HandleFunc("/v3/conversations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer legacy-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ok"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tr := newTransport(nil, srv.URL, srv.Client())
	creds := credentials{AppID: testAppID, TenantID: testTenantID, AppSecret: "s3cret"}
	conn := newConnector(creds, tr.tokens, srv.Client())
	for i := 0; i < 2; i++ {
		id, err := conn.SendMessage(context.Background(), srv.URL, "a:1", "hi", "")
		if err != nil || id != "ok" {
			t.Fatalf("send %d: id=%q err=%v", i, id, err)
		}
	}
	if legacyMints.Load() != 1 {
		t.Fatalf("legacy token minted %d times, want 1 (cached after fallback)", legacyMints.Load())
	}
}
