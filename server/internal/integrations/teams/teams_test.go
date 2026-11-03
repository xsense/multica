package teams

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	testAppID    = "11111111-2222-3333-4444-555555555555"
	testTenantID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func init() { allowInsecureServiceURLForTests = true }

// ---- fake Bot Framework metadata + Connector ----

type fakeBotFramework struct {
	t      *testing.T
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
	mu     sync.Mutex
	sent   []map[string]any
	paths  []string
	member teamsMember
	tokens int
	reject bool
}

func newFakeBotFramework(t *testing.T) *fakeBotFramework {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBotFramework{t: t, key: key, kid: "test-kid"}
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": f.server.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty":          "RSA",
			"kid":          f.kid,
			"n":            base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":            base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			"endorsements": []string{"msteams"},
		}}})
	})
	mux.HandleFunc("/"+testTenantID+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokens++
		reject := f.reject
		f.mu.Unlock()
		if reject || r.Form.Get("client_secret") != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "bot-token", "expires_in": 3600})
	})
	mux.HandleFunc("/v3/conversations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.EscapedPath())
		if strings.Contains(r.URL.Path, "/members/") {
			_ = json.NewEncoder(w).Encode(f.member)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sent = append(f.sent, body)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "act-" + string(rune('0'+len(f.sent)))})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBotFramework) token(t *testing.T, mutate func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":        botFrameworkIssuer,
		"aud":        testAppID,
		"exp":        time.Now().Add(time.Hour).Unix(),
		"nbf":        time.Now().Add(-time.Minute).Unix(),
		"serviceurl": f.server.URL,
	}
	if mutate != nil {
		mutate(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fakeBotFramework) transport() *Transport {
	return newTransport(nil, f.server.URL, f.server.Client())
}

func (f *fakeBotFramework) installConfig() []byte {
	b, _ := json.Marshal(installConfig{
		AppID:    testAppID,
		TenantID: testTenantID,
		BotName:  "Helper",
		// nil decrypter: the stored bytes are the plaintext.
		AppSecretEncrypted: base64.StdEncoding.EncodeToString([]byte("s3cret")),
	})
	return b
}

// ---- auth ----

func TestAuthenticatorAcceptsValidConnectorToken(t *testing.T) {
	f := newFakeBotFramework(t)
	a := NewAuthenticator(f.server.URL+"/openid", f.server.Client())
	if err := a.Verify(context.Background(), "Bearer "+f.token(t, nil), testAppID, "msteams", f.server.URL); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestAuthenticatorRejects(t *testing.T) {
	f := newFakeBotFramework(t)
	a := NewAuthenticator(f.server.URL+"/openid", f.server.Client())
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": botFrameworkIssuer, "aud": testAppID, "exp": time.Now().Add(time.Hour).Unix(), "serviceurl": f.server.URL,
	})
	forged.Header["kid"] = f.kid
	forgedStr, _ := forged.SignedString(other)

	cases := map[string]struct {
		header, channel, serviceURL string
	}{
		"missing header":   {"", "msteams", f.server.URL},
		"wrong audience":   {"Bearer " + f.token(t, func(c jwt.MapClaims) { c["aud"] = "99999999-2222-3333-4444-555555555555" }), "msteams", f.server.URL},
		"wrong issuer":     {"Bearer " + f.token(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }), "msteams", f.server.URL},
		"expired":          {"Bearer " + f.token(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), "msteams", f.server.URL},
		"no expiry":        {"Bearer " + f.token(t, func(c jwt.MapClaims) { delete(c, "exp") }), "msteams", f.server.URL},
		"serviceurl swap":  {"Bearer " + f.token(t, nil), "msteams", "https://smba.trafficmanager.net/apac/"},
		"no serviceurl":    {"Bearer " + f.token(t, func(c jwt.MapClaims) { delete(c, "serviceurl") }), "msteams", f.server.URL},
		"not endorsed":     {"Bearer " + f.token(t, nil), "webchat", f.server.URL},
		"forged signature": {"Bearer " + forgedStr, "msteams", f.server.URL},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := a.Verify(context.Background(), tc.header, testAppID, tc.channel, tc.serviceURL)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("want ErrUnauthenticated, got %v", err)
			}
		})
	}
}

// ---- activity translation ----

func baseActivity(serviceURL string) Activity {
	return Activity{
		Type:       "message",
		ID:         "1700000000000",
		ServiceURL: serviceURL,
		ChannelID:  "msteams",
		From:       ChannelAccount{ID: "29:user", Name: "Somchai", AADObjectID: "0f0f0f0f-0000-0000-0000-000000000001"},
		Conversation: Conversation{
			ID: "a:personal-conv", ConversationType: "personal", TenantID: testTenantID,
		},
		Recipient: ChannelAccount{ID: botIDPrefix + testAppID, Name: "Helper"},
		Text:      "สวัสดีครับ ช่วยดู PR หน่อย",
	}
}

func TestInboundPersonalMessage(t *testing.T) {
	msg, ok := inboundFromActivity(baseActivity("https://x"), testAppID, testTenantID)
	if !ok {
		t.Fatal("expected message to pass")
	}
	if msg.Source.ChatType != channel.ChatTypeP2P || !msg.AddressedToBot {
		t.Fatalf("p2p message should be addressed: %+v", msg.Source)
	}
	if msg.Text != "สวัสดีครับ ช่วยดู PR หน่อย" {
		t.Fatalf("text = %q", msg.Text)
	}
	if msg.Source.SenderID != "0f0f0f0f-0000-0000-0000-000000000001" {
		t.Fatalf("sender should be the AAD object id, got %q", msg.Source.SenderID)
	}
	raw, err := decodeRaw(msg)
	if err != nil || raw.FromID != "29:user" || raw.ServiceURL != "https://x" || raw.AppID != testAppID {
		t.Fatalf("raw = %+v, %v", raw, err)
	}
}

func TestInboundChannelMentionIsStrippedAndThreaded(t *testing.T) {
	a := baseActivity("https://x")
	a.Conversation = Conversation{ID: "19:abc@thread.tacv2;messageid=1699", ConversationType: "channel", TenantID: testTenantID}
	a.TextFormat = "xml"
	a.Text = "<at>Helper</at>&nbsp;/issue Fix login<br>details here and ask <at>Nok</at>"
	a.Entities = []Entity{
		{Type: "mention", Text: "<at>Helper</at>", Mentioned: &ChannelAccount{ID: botIDPrefix + testAppID, Name: "Helper"}},
		{Type: "mention", Text: "<at>Nok</at>", Mentioned: &ChannelAccount{ID: "29:nok", Name: "Nok"}},
	}
	msg, ok := inboundFromActivity(a, testAppID, testTenantID)
	if !ok {
		t.Fatal("expected message to pass")
	}
	if !msg.AddressedToBot || msg.Source.ChatType != channel.ChatTypeGroup {
		t.Fatalf("channel mention must be addressed group: %+v", msg)
	}
	if want := "/issue Fix login\ndetails here and ask @Nok"; msg.Text != want {
		t.Fatalf("text = %q, want %q", msg.Text, want)
	}
	if msg.Source.ThreadID != "1699" || msg.Source.ChatID != a.Conversation.ID {
		t.Fatalf("thread routing = %+v", msg.Source)
	}
}

func TestInboundGroupWithoutMentionIsNotAddressed(t *testing.T) {
	a := baseActivity("https://x")
	a.Conversation = Conversation{ID: "19:group@thread.v2", ConversationType: "groupChat", TenantID: testTenantID}
	msg, ok := inboundFromActivity(a, testAppID, testTenantID)
	if !ok || msg.AddressedToBot {
		t.Fatalf("unaddressed group chatter must pass through unaddressed: ok=%v addressed=%v", ok, msg.AddressedToBot)
	}
}

func TestInboundDropsBotsAndEmpty(t *testing.T) {
	a := baseActivity("https://x")
	a.From.ID = botIDPrefix + "other"
	if _, ok := inboundFromActivity(a, testAppID, testTenantID); ok {
		t.Fatal("bot sender must be dropped")
	}
	b := baseActivity("https://x")
	b.Text = "  "
	if _, ok := inboundFromActivity(b, testAppID, testTenantID); ok {
		t.Fatal("empty message must be dropped")
	}
	c := baseActivity("https://x")
	c.Text = ""
	c.Attachments = []Attachment{{ContentType: "application/vnd.microsoft.teams.file.download.info", Name: "a.pdf"}}
	msg, ok := inboundFromActivity(c, testAppID, testTenantID)
	if !ok || msg.Type != channel.MsgTypeUnknown {
		t.Fatalf("file-only message should surface as unknown type: ok=%v type=%v", ok, msg.Type)
	}
}

func TestInboundControlCommand(t *testing.T) {
	a := baseActivity("https://x")
	a.Text = "/clear"
	msg, ok := inboundFromActivity(a, testAppID, testTenantID)
	if !ok || !msg.ForceFresh || msg.CommandText != "/clear" {
		t.Fatalf("/clear: ok=%v fresh=%v cmd=%q", ok, msg.ForceFresh, msg.CommandText)
	}
}

// ---- webhook ----

type fakeWebhookQueries struct {
	inst db.ChannelInstallation
	miss bool
}

func (q *fakeWebhookQueries) GetChannelInstallationByAppID(_ context.Context, arg db.GetChannelInstallationByAppIDParams) (db.ChannelInstallation, error) {
	if q.miss || arg.AppID != testAppID || arg.ChannelType != string(TypeTeams) {
		return db.ChannelInstallation{}, pgx.ErrNoRows
	}
	return q.inst, nil
}

func newTestWebhook(f *fakeBotFramework, handled *[]channel.InboundMessage) *Webhook {
	return &Webhook{
		q:         &fakeWebhookQueries{inst: db.ChannelInstallation{Config: f.installConfig(), Status: "active"}},
		transport: f.transport(),
		auth:      NewAuthenticator(f.server.URL+"/openid", f.server.Client()),
		handle: func(_ context.Context, msg channel.InboundMessage) error {
			*handled = append(*handled, msg)
			return nil
		},
		logger: discardLogger(),
		spawn:  func(fn func()) { fn() },
	}
}

func post(t *testing.T, h http.Handler, act Activity, token string) int {
	t.Helper()
	body, _ := json.Marshal(act)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/teams", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhookDeliversAuthenticatedMessage(t *testing.T) {
	f := newFakeBotFramework(t)
	var handled []channel.InboundMessage
	w := newTestWebhook(f, &handled)
	if code := post(t, w, baseActivity(f.server.URL), f.token(t, nil)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(handled) != 1 || handled[0].Source.ChannelType != TypeTeams {
		t.Fatalf("handled = %+v", handled)
	}
}

func TestWebhookRejectsUnauthenticatedAndForeignTenant(t *testing.T) {
	f := newFakeBotFramework(t)
	var handled []channel.InboundMessage
	w := newTestWebhook(f, &handled)
	if code := post(t, w, baseActivity(f.server.URL), ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d", code)
	}
	foreign := baseActivity(f.server.URL)
	foreign.Conversation.TenantID = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"
	if code := post(t, w, foreign, f.token(t, nil)); code != http.StatusForbidden {
		t.Fatalf("foreign tenant: status = %d", code)
	}
	unknown := baseActivity(f.server.URL)
	unknown.Recipient.ID = botIDPrefix + "99999999-2222-3333-4444-555555555555"
	if code := post(t, w, unknown, f.token(t, nil)); code != http.StatusUnauthorized {
		t.Fatalf("unknown bot: status = %d", code)
	}
	if len(handled) != 0 {
		t.Fatalf("nothing should reach the engine, got %d", len(handled))
	}
}

func TestWebhookEngineErrorAsks5xx(t *testing.T) {
	f := newFakeBotFramework(t)
	w := newTestWebhook(f, new([]channel.InboundMessage))
	w.handle = func(context.Context, channel.InboundMessage) error { return errors.New("db down") }
	if code := post(t, w, baseActivity(f.server.URL), f.token(t, nil)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d", code)
	}
}

func TestWebhookWelcomesOnPersonalInstall(t *testing.T) {
	f := newFakeBotFramework(t)
	w := newTestWebhook(f, new([]channel.InboundMessage))
	a := baseActivity(f.server.URL)
	a.Type = "conversationUpdate"
	a.Text = ""
	a.MembersAdded = []ChannelAccount{{ID: botIDPrefix + testAppID}}
	if code := post(t, w, a, f.token(t, nil)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0]["text"].(string), "/issue") {
		t.Fatalf("welcome not sent: %+v", f.sent)
	}
}

// ---- connector / install ----

func TestConnectorSendsMarkdownAndCachesToken(t *testing.T) {
	f := newFakeBotFramework(t)
	conn, _, err := f.transport().connectorFor(f.installConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := conn.SendMessage(context.Background(), f.server.URL, "19:abc@thread.tacv2;messageid=1", "**hi**", ""); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokens != 1 {
		t.Fatalf("token minted %d times, want 1", f.tokens)
	}
	if f.sent[0]["textFormat"] != "markdown" || f.sent[0]["type"] != "message" {
		t.Fatalf("activity = %+v", f.sent[0])
	}
}

func TestTokenRejection(t *testing.T) {
	f := newFakeBotFramework(t)
	_, err := f.transport().tokens.mint(context.Background(), credentials{AppID: testAppID, TenantID: testTenantID, AppSecret: "wrong"})
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("want ErrTokenRejected, got %v", err)
	}
}

func TestValidServiceURLRejectsForeignHosts(t *testing.T) {
	allowInsecureServiceURLForTests = false
	defer func() { allowInsecureServiceURLForTests = true }()
	for _, ok := range []string{"https://smba.trafficmanager.net/apac/", "https://smba.trafficmanager.net/amer/"} {
		if _, err := validServiceURL(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://smba.trafficmanager.net/apac/",
		"https://evil.example/",
		"https://smba.trafficmanager.net.evil.example/",
		"https://smbaevil.trafficmanager.net/",
		"https://smba.trafficmanager.net:8443/apac/",
		"https://user@smba.trafficmanager.net/apac/",
	} {
		if _, err := validServiceURL(bad); err == nil {
			t.Fatalf("%s should be rejected", bad)
		}
	}
}

// ---- helpers ----

func TestMemberEmail(t *testing.T) {
	cases := map[string]teamsMember{
		"somchai@xsense.co.th": {Email: "Somchai@XSense.co.th"},
		"nok@ttthi.co.th":      {UserPrincipalName: "nok@ttthi.co.th"},
		"":                     {UserPrincipalName: "guest_gmail.com#EXT#@xsense.onmicrosoft.com"},
	}
	for want, m := range cases {
		if got := memberEmail(m); got != want {
			t.Fatalf("memberEmail(%+v) = %q, want %q", m, got, want)
		}
	}
}

func TestChunkMarkdownKeepsFences(t *testing.T) {
	long := "intro\n```go\n" + strings.Repeat("fmt.Println(1)\n", 40) + "```\noutro"
	chunks := chunkMarkdown(long, 200)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if runeLen(c) > 200+4 {
			t.Fatalf("chunk %d too long: %d", i, runeLen(c))
		}
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unbalanced fence:\n%s", i, c)
		}
	}
	if got := chunkMarkdown("short", 200); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text = %v", got)
	}
}

func TestBuildAppPackage(t *testing.T) {
	pkg, err := BuildAppPackage(ManifestParams{AppID: strings.ToUpper(testAppID), BotName: "Helper", AppURL: "https://multica.ttthi.co.th"})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	var m manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Bots[0].BotID != testAppID || m.ID != testAppID || m.ValidDomains[0] != "multica.ttthi.co.th" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(files["color.png"]) == 0 || len(files["outline.png"]) == 0 {
		t.Fatal("icons missing")
	}
	if _, err := BuildAppPackage(ManifestParams{AppID: testAppID, AppURL: "http://insecure"}); err == nil {
		t.Fatal("http app url must be rejected")
	}
}

func TestNormalizeGUID(t *testing.T) {
	if g, ok := normalizeGUID(" 11111111-2222-3333-4444-55555555555A "); !ok || g != "11111111-2222-3333-4444-55555555555a" {
		t.Fatalf("got %q %v", g, ok)
	}
	if _, ok := normalizeGUID("not-a-guid"); ok {
		t.Fatal("bad guid accepted")
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
