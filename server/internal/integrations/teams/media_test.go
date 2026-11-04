package teams

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ---- inbound translation ----

func downloadInfo(t *testing.T, url string) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(fileDownloadInfo{DownloadURL: url, UniqueID: "u-1", FileType: "pdf"})
	return b
}

func TestInboundPastedImageAndPersonalFile(t *testing.T) {
	allowInsecureServiceURLForTests = false
	defer func() { allowInsecureServiceURLForTests = true }()
	a := baseActivity("https://smba.trafficmanager.net/apac/")
	a.Text = "ดูรูปนี้หน่อย"
	a.Attachments = []Attachment{
		{ContentType: "image/*", ContentURL: "https://smba.trafficmanager.net/apac/v3/attachments/abc/views/original"},
		{ContentType: fileDownloadInfoType, Name: "spec.pdf", ContentURL: "https://x-my.sharepoint.com/personal/spec.pdf",
			Content: downloadInfo(t, "https://xsense-my.sharepoint.com/personal/download.aspx?tempauth=abc")},
		{ContentType: "text/html", Content: json.RawMessage(`"<p>ดูรูปนี้หน่อย</p>"`)},
	}
	msg, ok := inboundFromActivityWithMedia(a, testAppID, testTenantID, true)
	if !ok {
		t.Fatal("dropped")
	}
	if want := "[Image]\n[File: spec.pdf]\nดูรูปนี้หน่อย"; msg.Text != want {
		t.Fatalf("text = %q, want %q", msg.Text, want)
	}
	if msg.CommandText != "ดูรูปนี้หน่อย" || msg.Type != channel.MsgTypeFile {
		t.Fatalf("command=%q type=%v", msg.CommandText, msg.Type)
	}
	raw, _ := decodeRaw(msg)
	if len(raw.Media) != 2 || !raw.Media[0].BotAuth || raw.Media[1].BotAuth || raw.UnsupportedFile {
		t.Fatalf("raw media = %+v unsupported=%v", raw.Media, raw.UnsupportedFile)
	}
}

func TestInboundChannelFileIsUnsupportedButTextGoesOn(t *testing.T) {
	allowInsecureServiceURLForTests = false
	defer func() { allowInsecureServiceURLForTests = true }()
	a := baseActivity("https://smba.trafficmanager.net/apac/")
	a.Conversation = Conversation{ID: "19:c@thread.tacv2;messageid=1", ConversationType: "channel", TenantID: testTenantID}
	a.Text = "ช่วยดูไฟล์"
	a.Attachments = []Attachment{
		{ContentType: "reference", ContentURL: "https://xsense.sharepoint.com/sites/team/doc.docx", Name: "doc.docx"},
		{ContentType: "image/png", ContentURL: "https://graph.microsoft.com/v1.0/chats/x/messages/y/hostedContents/z/$value"},
	}
	msg, ok := inboundFromActivityWithMedia(a, testAppID, testTenantID, true)
	if !ok || msg.Text != "ช่วยดูไฟล์" || msg.Type != channel.MsgTypeText {
		t.Fatalf("ok=%v text=%q type=%v", ok, msg.Text, msg.Type)
	}
	raw, _ := decodeRaw(msg)
	if !raw.UnsupportedFile || len(raw.Media) != 0 {
		t.Fatalf("raw = %+v", raw)
	}
}

func TestInboundMediaWithoutStorageIsUnsupported(t *testing.T) {
	a := baseActivity("https://x")
	a.Text = ""
	a.Attachments = []Attachment{{ContentType: "image/png", ContentURL: "http://127.0.0.1/img"}}
	msg, ok := inboundFromActivityWithMedia(a, testAppID, testTenantID, false)
	if !ok || msg.Type != channel.MsgTypeUnknown {
		t.Fatalf("ok=%v type=%v", ok, msg.Type)
	}
}

func TestComposeMediaBodyIndexesRepeatedPlaceholders(t *testing.T) {
	media := []inboundMedia{{Placeholder: "[Image]"}, {Placeholder: "[Image]"}, {Placeholder: "[File: a]"}}
	body := composeMediaBody(media, "[Image] typed by user")
	if body != "[Image]\n[Image]\n[File: a]\n[Image] typed by user" {
		t.Fatalf("body = %q", body)
	}
	if media[0].PlaceholderIndex != 0 || media[1].PlaceholderIndex != 1 || media[2].PlaceholderIndex != 0 {
		t.Fatalf("indexes = %d %d %d", media[0].PlaceholderIndex, media[1].PlaceholderIndex, media[2].PlaceholderIndex)
	}
}

func TestMediaHostRules(t *testing.T) {
	allowInsecureServiceURLForTests = false
	defer func() { allowInsecureServiceURLForTests = true }()
	for _, ok := range []string{"https://smba.trafficmanager.net/apac/v3/attachments/a", "https://us-api.asm.skype.com/v1/objects/0-x/views/imgo"} {
		if !botAttachmentURL(ok) {
			t.Fatalf("%s should take the bot token", ok)
		}
	}
	for _, bad := range []string{"https://evil.example/a", "https://graph.microsoft.com/x", "https://asm.skype.com.evil.example/x", "http://smba.trafficmanager.net/a"} {
		if botAttachmentURL(bad) {
			t.Fatalf("%s must never receive the bot token", bad)
		}
	}
	if !sharePointDownloadURL("https://xsense-my.sharepoint.com/personal/x") || sharePointDownloadURL("https://sharepoint.com.evil.example/x") {
		t.Fatal("sharepoint rule wrong")
	}
}

// ---- resolver ----

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]byte{}, types: map[string]string{}}
}

func (s *memStore) Upload(_ context.Context, key string, data []byte, contentType, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key], s.types[key] = data, contentType
	return s.ObjectURL(key), nil
}
func (s *memStore) ObjectURL(key string) string { return "https://files.example/uploads/" + key }
func (s *memStore) KeyFromURL(u string) string {
	return strings.TrimPrefix(u, "https://files.example/uploads/")
}
func (s *memStore) GetReader(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return io.NopCloser(bytes.NewReader(s.objects[key])), nil
}

type okLedger struct{ n int }

func (l *okLedger) RecordPendingMediaObject(context.Context, engine.RecordPendingMediaObjectParams) (bool, error) {
	l.n++
	return true, nil
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestResolveMediaDownloadsWithTheRightCredentials(t *testing.T) {
	f := newFakeBotFramework(t)
	pic := pngBytes(t, 8, 8)
	var sawPictureAuth, sawFileAuth string
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pic":
			sawPictureAuth = r.Header.Get("Authorization")
			_, _ = w.Write(pic)
		case "/doc":
			sawFileAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4"))
		}
	}))
	defer files.Close()

	store, ledger := newMemStore(), &okLedger{}
	r := &mediaResolver{transport: f.transport(), storage: store, ledger: ledger, client: newMediaClient(files.Client()), logger: discardLogger()}
	a := baseActivity(f.server.URL)
	a.Text = "two files"
	a.Attachments = []Attachment{
		{ContentType: "image/*", ContentURL: files.URL + "/pic"},
		{ContentType: fileDownloadInfoType, Name: "spec.pdf", Content: downloadInfo(t, files.URL+"/doc")},
	}
	msg, ok := inboundFromActivityWithMedia(a, testAppID, testTenantID, true)
	if !ok || !r.HasMedia(msg) {
		t.Fatal("expected media")
	}
	inst := engine.ResolvedInstallation{ID: uuidOf(9), WorkspaceID: uuidOf(8), Platform: db.ChannelInstallation{Config: f.installConfig()}}
	out := r.ResolveMedia(context.Background(), inst, engine.ResolvedIdentity{}, pgtype.UUID{}, uuidOf(7), msg)
	if len(out.MediaRefs) != 2 || ledger.n != 2 {
		t.Fatalf("refs=%d ledger=%d", len(out.MediaRefs), ledger.n)
	}
	if out.MediaRefs[0].MimeType != "image/png" || out.MediaRefs[0].InlinePlaceholder != "[Image]" {
		t.Fatalf("picture ref = %+v", out.MediaRefs[0])
	}
	if out.MediaRefs[1].Filename != "spec.pdf" || out.MediaRefs[1].MimeType != "application/pdf" {
		t.Fatalf("file ref = %+v", out.MediaRefs[1])
	}
	if sawPictureAuth != "Bearer bot-token" {
		t.Fatalf("picture fetched with %q, want the bot token", sawPictureAuth)
	}
	if sawFileAuth != "" {
		t.Fatalf("SharePoint link must be fetched without credentials, got %q", sawFileAuth)
	}
}

// ---- outbound ----

func TestFitInlineImage(t *testing.T) {
	small := pngBytes(t, 40, 30)
	out, ct, err := fitInlineImage(small, "image/png")
	if err != nil || ct != "image/png" || !bytes.Equal(out, small) {
		t.Fatalf("small image should pass unchanged: ct=%s err=%v", ct, err)
	}
	big := pngBytes(t, 2400, 1200)
	out, _, err = fitInlineImage(big, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 1024 || cfg.Height != 512 || len(out) > maxInlineImageBytes {
		t.Fatalf("scaled = %dx%d, %d bytes, err=%v", cfg.Width, cfg.Height, len(out), err)
	}
	if _, _, err := fitInlineImage([]byte("not an image"), "image/png"); err == nil {
		t.Fatal("garbage must not be sent inline")
	}
}

type fakeOutboundQueries struct {
	attachments []db.Attachment
}

func (q *fakeOutboundQueries) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return db.AgentTaskQueue{}, nil
}
func (q *fakeOutboundQueries) TaskHasChannelIngestedMessages(context.Context, pgtype.UUID) (bool, error) {
	return true, nil
}
func (q *fakeOutboundQueries) GetChannelTaskDelivery(context.Context, pgtype.UUID) (db.ChannelTaskDelivery, error) {
	return db.ChannelTaskDelivery{}, nil
}
func (q *fakeOutboundQueries) GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	return db.ChannelInstallation{}, nil
}
func (q *fakeOutboundQueries) SetChatMessageChannelOutboundProvenanceByTask(context.Context, db.SetChatMessageChannelOutboundProvenanceByTaskParams) (int64, error) {
	return 1, nil
}
func (q *fakeOutboundQueries) RecordChannelOutboundMessage(context.Context, db.RecordChannelOutboundMessageParams) error {
	return nil
}
func (q *fakeOutboundQueries) ListAttachmentsByChatMessage(context.Context, db.ListAttachmentsByChatMessageParams) ([]db.Attachment, error) {
	return q.attachments, nil
}
func (q *fakeOutboundQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{Slug: "xsense-software"}, nil
}

func TestDeliverAttachmentsInlinesPicturesAndLinksFiles(t *testing.T) {
	f := newFakeBotFramework(t)
	store := newMemStore()
	_, _ = store.Upload(context.Background(), "a/pic.png", pngBytes(t, 16, 16), "image/png", "pic.png")
	_, _ = store.Upload(context.Background(), "a/report.pdf", []byte("%PDF"), "application/pdf", "report.pdf")
	q := &fakeOutboundQueries{attachments: []db.Attachment{
		{ID: uuidOf(1), Filename: "pic.png", Url: store.ObjectURL("a/pic.png"), ContentType: "image/png", SizeBytes: 100},
		{ID: uuidOf(2), Filename: "report_[final].pdf", Url: store.ObjectURL("a/report.pdf"), ContentType: "application/pdf", SizeBytes: 2 << 20},
	}}
	o := NewOutbound(q, f.transport(), discardLogger())
	o.EnableFileDelivery(store, "https://multica.ttthi.co.th/")
	conn, _, err := f.transport().connectorFor(f.installConfig())
	if err != nil {
		t.Fatal(err)
	}
	e := events.Event{
		WorkspaceID:   "00000000-0000-0000-0000-000000000008",
		ChatSessionID: "11111111-0000-0000-0000-000000000001",
		Payload:       protocol.ChatDonePayload{MessageID: "22222222-0000-0000-0000-000000000002"},
	}
	o.deliverAttachments(context.Background(), conn, replyTarget{ServiceURL: f.server.URL, ConversationID: "a:1"}, e)

	if len(f.sent) != 2 {
		t.Fatalf("sent %d activities, want 2: %+v", len(f.sent), f.sent)
	}
	atts, _ := f.sent[0]["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("first activity should carry the picture: %+v", f.sent[0])
	}
	att := atts[0].(map[string]any)
	if att["contentType"] != "image/png" || !strings.HasPrefix(att["contentUrl"].(string), "data:image/png;base64,") {
		t.Fatalf("attachment = %+v", att)
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(att["contentUrl"].(string), "data:image/png;base64,")); err != nil {
		t.Fatal("data URI is not valid base64")
	}
	text, _ := f.sent[1]["text"].(string)
	for _, want := range []string{`report\_\[final\].pdf`, "2.0 MB", "https://multica.ttthi.co.th/xsense-software/chat?session=11111111-0000-0000-0000-000000000001"} {
		if !strings.Contains(text, want) {
			t.Fatalf("file message missing %q:\n%s", want, text)
		}
	}
}
