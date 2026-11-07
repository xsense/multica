package teams

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	pushWorkspaceID = "01a1b55a-0000-7000-8000-000000000001"
	pushRecipientID = "01a1b55a-0000-7000-8000-000000000002"
	pushAgentID     = "01a1b55a-0000-7000-8000-000000000003"
	pushIssueID     = "01a1b55a-0000-7000-8000-000000000004"
	pushInstallID   = "01a1b55a-0000-7000-8000-000000000005"
)

func inboxItem(kind, actorType, status string) map[string]any {
	body := "ตรวจ backfill แล้ว ผ่านทั้งหมด"
	issueID := pushIssueID
	return map[string]any{
		"id":             "item-1",
		"workspace_id":   pushWorkspaceID,
		"recipient_type": "member",
		"recipient_id":   pushRecipientID,
		"type":           kind,
		"issue_id":       &issueID,
		"title":          "SLCC: location ค้างค่าเดิม",
		"body":           &body,
		"actor_type":     &actorType,
		"actor_id":       ptr(pushAgentID),
		"issue_status":   status,
	}
}

func ptr(s string) *string { return &s }

func TestShouldPushInbox(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
		want bool
	}{
		{"agent comment", inboxItem("new_comment", "agent", "in_progress"), true},
		{"agent mention", inboxItem("mentioned", "agent", "todo"), true},
		{"member comment", inboxItem("new_comment", "member", "in_progress"), false},
		{"status in_review", inboxItem("status_changed", "agent", "in_review"), true},
		{"status done by member", inboxItem("status_changed", "member", "done"), true},
		{"status blocked", inboxItem("status_changed", "agent", "blocked"), true},
		{"status in_progress", inboxItem("status_changed", "agent", "in_progress"), false},
		{"assignee changed", inboxItem("assignee_changed", "agent", "todo"), false},
		{"agent recipient", func() map[string]any {
			it := inboxItem("new_comment", "agent", "todo")
			it["recipient_type"] = "agent"
			return it
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPushInbox(tc.item); got != tc.want {
				t.Fatalf("shouldPushInbox = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildInboxText(t *testing.T) {
	got := buildInboxText(inboxItem("status_changed", "agent", "done"), "https://multica.example", "xsense-software")
	for _, want := range []string{
		"เสร็จแล้ว (done)",
		"SLCC: location ค้างค่าเดิม",
		"ตรวจ backfill แล้ว",
		"[เปิดใน Multica](https://multica.example/xsense-software/issues/" + pushIssueID + ")",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text missing %q:\n%s", want, got)
		}
	}

	item := inboxItem("new_comment", "agent", "todo")
	item["title"] = "[click](https://evil.example)"
	item["body"] = strings.Repeat("ก", inboxBodyMaxRunes+50)
	got = buildInboxText(item, "", "")
	if strings.Contains(got, "[click](") {
		t.Fatalf("title link not escaped: %s", got)
	}
	if strings.Contains(got, "เปิดใน Multica") {
		t.Fatalf("link rendered without appURL: %s", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long body not truncated: %s", got)
	}
	if buildInboxText(map[string]any{"type": "new_comment"}, "", "") != "" {
		t.Fatal("item without title should render nothing")
	}
}

// ---- end to end against a fake Connector ----

type fakeConnector struct {
	server   *httptest.Server
	mu       sync.Mutex
	created  []createConversationRequest
	messages []map[string]any
	paths    []string
}

func newFakeConnector(t *testing.T) *fakeConnector {
	t.Helper()
	f := &fakeConnector{}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+testTenantID+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "bot-token", "expires_in": 3600})
	})
	mux.HandleFunc("/v3/conversations", func(w http.ResponseWriter, r *http.Request) {
		var req createConversationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.created = append(f.created, req)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "a:personal-conv"})
	})
	mux.HandleFunc("/v3/conversations/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.messages = append(f.messages, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "act-1"})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type fakePushQueries struct {
	agentBinding  *db.ChannelUserBinding
	memberBinding *db.ChannelUserBinding
	inst          db.ChannelInstallation
}

func (q *fakePushQueries) FindChannelBindingForMember(context.Context, db.FindChannelBindingForMemberParams) (db.ChannelUserBinding, error) {
	if q.memberBinding == nil {
		return db.ChannelUserBinding{}, pgx.ErrNoRows
	}
	return *q.memberBinding, nil
}

func (q *fakePushQueries) FindChannelBindingForMemberOnAgentInstallation(context.Context, db.FindChannelBindingForMemberOnAgentInstallationParams) (db.ChannelUserBinding, error) {
	if q.agentBinding == nil {
		return db.ChannelUserBinding{}, pgx.ErrNoRows
	}
	return *q.agentBinding, nil
}

func (q *fakePushQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return q.inst, nil
}

func (q *fakePushQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{Slug: "xsense-software"}, nil
}

func pushBinding(serviceURL string) *db.ChannelUserBinding {
	cfg, _ := json.Marshal(map[string]string{"bound_by": "email", "service_url": serviceURL, "teams_id": "29:user"})
	return &db.ChannelUserBinding{
		InstallationID: util.MustParseUUID(pushInstallID),
		ChannelType:    string(TypeTeams),
		ChannelUserID:  "0f0f0f0f-0000-0000-0000-000000000001",
		Config:         cfg,
	}
}

func newTestPush(t *testing.T, f *fakeConnector, q *fakePushQueries) *InboxPush {
	t.Helper()
	installCfg := (&fakeBotFramework{}).installConfig()
	q.inst = db.ChannelInstallation{Status: "active", Config: installCfg}
	p := NewInboxPush(q, newTransport(nil, f.server.URL, f.server.Client()), "https://multica.example", nil)
	p.spawn = func(fn func()) { fn() }
	return p
}

func publishInbox(p *InboxPush, item map[string]any) {
	bus := events.New()
	p.Register(bus)
	bus.Publish(events.Event{Type: protocol.EventInboxNew, Payload: map[string]any{"item": item}})
}

func TestInboxPushSendsToPersonalChat(t *testing.T) {
	f := newFakeConnector(t)
	q := &fakePushQueries{agentBinding: pushBinding(f.server.URL)}
	publishInbox(newTestPush(t, f, q), inboxItem("new_comment", "agent", "in_progress"))

	if len(f.created) != 1 {
		t.Fatalf("create conversation calls = %d, want 1", len(f.created))
	}
	c := f.created[0]
	if c.Bot.ID != botIDPrefix+testAppID || len(c.Members) != 1 || c.Members[0].ID != "29:user" || c.TenantID != testTenantID {
		t.Fatalf("unexpected create request: %+v", c)
	}
	if len(f.messages) != 1 || f.paths[0] != "/v3/conversations/a:personal-conv/activities" {
		t.Fatalf("messages=%d paths=%v", len(f.messages), f.paths)
	}
	if text, _ := f.messages[0]["text"].(string); !strings.Contains(text, "agent ตอบใน issue") {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestInboxPushSkips(t *testing.T) {
	cases := map[string]struct {
		q    *fakePushQueries
		item map[string]any
	}{
		"filtered type":   {&fakePushQueries{memberBinding: pushBinding("")}, inboxItem("new_comment", "member", "todo")},
		"unbound member":  {&fakePushQueries{}, inboxItem("new_comment", "agent", "todo")},
		"no route stored": {&fakePushQueries{memberBinding: pushBinding("")}, inboxItem("status_changed", "agent", "done")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeConnector(t)
			publishInbox(newTestPush(t, f, tc.q), tc.item)
			if len(f.created)+len(f.messages) != 0 {
				t.Fatalf("expected no Connector calls, got %d creates %d messages", len(f.created), len(f.messages))
			}
		})
	}
}

func TestInboxPushFallsBackToMemberBinding(t *testing.T) {
	f := newFakeConnector(t)
	q := &fakePushQueries{memberBinding: pushBinding(f.server.URL)}
	publishInbox(newTestPush(t, f, q), inboxItem("status_changed", "member", "blocked"))
	if len(f.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(f.messages))
	}
}

// ---- route capture on inbound ----

type routeIdentityQueries struct {
	binding db.ChannelUserBinding
	upserts []db.CreateChannelUserBindingParams
}

func (q *routeIdentityQueries) GetChannelUserBindingByUserID(context.Context, db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error) {
	return q.binding, nil
}

func (q *routeIdentityQueries) GetMemberByUserAndWorkspace(context.Context, db.GetMemberByUserAndWorkspaceParams) (db.Member, error) {
	return db.Member{}, nil
}

func (q *routeIdentityQueries) GetUserByEmail(context.Context, string) (db.User, error) {
	return db.User{}, pgx.ErrNoRows
}

func (q *routeIdentityQueries) CreateChannelUserBinding(_ context.Context, arg db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error) {
	q.upserts = append(q.upserts, arg)
	return db.ChannelUserBinding{}, nil
}

func inboundWithRoute(t *testing.T, serviceURL string) channel.InboundMessage {
	t.Helper()
	raw, _ := json.Marshal(teamsRawEvent{ServiceURL: serviceURL, FromID: "29:user"})
	return channel.InboundMessage{Source: channel.Source{SenderID: "aad-1"}, Raw: raw}
}

func TestResolveSenderRemembersRoute(t *testing.T) {
	q := &routeIdentityQueries{binding: db.ChannelUserBinding{ChannelType: string(TypeTeams), ChannelUserID: "aad-1", Config: []byte(`{}`)}}
	r := &identityResolver{q: q, logger: newDiscardLogger()}
	if _, err := r.ResolveSender(context.Background(), engineInst(), inboundWithRoute(t, "https://smba.example/apac/")); err != nil {
		t.Fatal(err)
	}
	if len(q.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(q.upserts))
	}
	if got := routeFromBinding(q.upserts[0].Config); got != (userRoute{ServiceURL: "https://smba.example/apac/", TeamsID: "29:user"}) {
		t.Fatalf("stored route = %+v", got)
	}

	// Unchanged route: no write.
	q.binding.Config = q.upserts[0].Config
	q.upserts = nil
	if _, err := r.ResolveSender(context.Background(), engineInst(), inboundWithRoute(t, "https://smba.example/apac/")); err != nil {
		t.Fatal(err)
	}
	if len(q.upserts) != 0 {
		t.Fatalf("unchanged route rewrote the binding")
	}
}

func engineInst() engine.ResolvedInstallation {
	return engine.ResolvedInstallation{ID: util.MustParseUUID(pushInstallID), WorkspaceID: util.MustParseUUID(pushWorkspaceID)}
}

func newDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
