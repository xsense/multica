package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file pushes selected inbox notifications into the recipient's
// personal chat with a Teams bot, so work that finishes on an issue (a
// scheduled wakeup, an autopilot, an assignment) reaches the person who
// asked for it in Teams — the shape the WeCom adapter uses for inbox:new.
//
// Only what tells the member that work came back is pushed: comments and
// mentions written by an agent, and status changes into in_review, done or
// blocked. Everything else stays in the Multica inbox only.

// inboxPushQueries is what the push needs; *db.Queries satisfies it.
type inboxPushQueries interface {
	FindChannelBindingForMember(ctx context.Context, arg db.FindChannelBindingForMemberParams) (db.ChannelUserBinding, error)
	FindChannelBindingForMemberOnAgentInstallation(ctx context.Context, arg db.FindChannelBindingForMemberOnAgentInstallationParams) (db.ChannelUserBinding, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
}

// InboxPush is the inbox:new subscriber.
type InboxPush struct {
	q         inboxPushQueries
	transport *Transport
	appURL    string
	logger    *slog.Logger
	spawn     func(func())
}

// NewInboxPush builds the subscriber. appURL is the web origin used for the
// "open in Multica" link; empty omits the link.
func NewInboxPush(q inboxPushQueries, transport *Transport, appURL string, logger *slog.Logger) *InboxPush {
	if logger == nil {
		logger = slog.Default()
	}
	return &InboxPush{
		q:         q,
		transport: transport,
		appURL:    strings.TrimRight(appURL, "/"),
		logger:    logger,
		spawn:     func(f func()) { go f() },
	}
}

// Register subscribes to inbox:new.
func (p *InboxPush) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventInboxNew, p.handle)
}

func (p *InboxPush) handle(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	item, ok := payload["item"].(map[string]any)
	if !ok || !shouldPushInbox(item) {
		return
	}
	// Bus delivery is synchronous; never hold the publisher on a Teams call.
	p.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), detachedTimeout)
		defer cancel()
		if err := p.deliver(ctx, item); err != nil {
			p.logger.WarnContext(ctx, "teams inbox push failed",
				"error", err, "inbox_item_id", stringField(item["id"]))
		}
	})
}

// pushStatuses are the status changes worth a Teams message: the issue is
// waiting on the member, finished, or stuck.
var pushStatuses = map[string]bool{"in_review": true, "done": true, "blocked": true}

// shouldPushInbox decides whether an inbox item goes to Teams.
func shouldPushInbox(item map[string]any) bool {
	if stringField(item["recipient_type"]) != "member" {
		return false
	}
	switch stringField(item["type"]) {
	case "new_comment", "mentioned":
		return stringField(item["actor_type"]) == "agent"
	case "status_changed":
		return pushStatuses[stringField(item["issue_status"])]
	}
	return false
}

func (p *InboxPush) deliver(ctx context.Context, item map[string]any) error {
	recipientID, err := util.ParseUUID(stringField(item["recipient_id"]))
	if err != nil || !recipientID.Valid {
		return nil
	}
	workspaceID, err := util.ParseUUID(stringField(item["workspace_id"]))
	if err != nil || !workspaceID.Valid {
		return nil
	}
	binding, err := p.bindingFor(ctx, item, workspaceID, recipientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not bound to any Teams bot
		}
		return fmt.Errorf("lookup member binding: %w", err)
	}
	route := routeFromBinding(binding.Config)
	if route.ServiceURL == "" {
		// Bound before routes were recorded; set on their next message.
		p.logger.DebugContext(ctx, "teams inbox push: binding has no route yet",
			"installation_id", util.UUIDToString(binding.InstallationID))
		return nil
	}
	userID := route.TeamsID
	if userID == "" {
		userID = binding.ChannelUserID
	}
	inst, err := p.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          binding.InstallationID,
		ChannelType: string(TypeTeams),
	})
	if err != nil {
		return fmt.Errorf("load teams installation: %w", err)
	}
	if inst.Status != "active" {
		return nil
	}
	conn, _, err := p.transport.connectorFor(inst.Config)
	if err != nil {
		return fmt.Errorf("decode teams credentials: %w", err)
	}

	slug := ""
	if ws, err := p.q.GetWorkspace(ctx, workspaceID); err == nil {
		slug = ws.Slug
	}
	text := buildInboxText(item, p.appURL, slug)
	if text == "" {
		return nil
	}

	convID, err := conn.CreatePersonalConversation(ctx, route.ServiceURL, userID)
	if err != nil {
		return fmt.Errorf("open personal chat: %w", err)
	}
	if _, err := sendWithRetry(ctx, func(ctx context.Context) (string, error) {
		return conn.SendMessage(ctx, route.ServiceURL, convID, text, "")
	}); err != nil {
		return fmt.Errorf("post inbox message: %w", err)
	}
	return nil
}

// bindingFor prefers the bot of the agent that raised the notification, so a
// member bound to several bots hears about each agent's work from that
// agent's bot; otherwise the member's most recently bound Teams bot.
func (p *InboxPush) bindingFor(ctx context.Context, item map[string]any, workspaceID, recipientID pgtype.UUID) (db.ChannelUserBinding, error) {
	if stringField(item["actor_type"]) == "agent" {
		if agentID, err := util.ParseUUID(stringField(item["actor_id"])); err == nil && agentID.Valid {
			b, err := p.q.FindChannelBindingForMemberOnAgentInstallation(ctx, db.FindChannelBindingForMemberOnAgentInstallationParams{
				WorkspaceID:   workspaceID,
				MulticaUserID: recipientID,
				ChannelType:   string(TypeTeams),
				AgentID:       agentID,
			})
			if err == nil {
				return b, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				p.logger.WarnContext(ctx, "teams inbox push: agent bot lookup failed", "error", err)
			}
		}
	}
	return p.q.FindChannelBindingForMember(ctx, db.FindChannelBindingForMemberParams{
		WorkspaceID:   workspaceID,
		MulticaUserID: recipientID,
		ChannelType:   string(TypeTeams),
	})
}

// inboxBodyMaxRunes keeps a notification a pointer to the issue, not a copy
// of the comment.
const inboxBodyMaxRunes = 1500

// buildInboxText renders the Teams message. Title and body are user/agent
// text, so they are escaped: the message comes from a bot the reader trusts
// and must not carry links the author smuggled in.
func buildInboxText(item map[string]any, appURL, slug string) string {
	title := strings.TrimSpace(stringField(item["title"]))
	if title == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("🔔 **")
	b.WriteString(inboxHeadline(item))
	b.WriteString("**: ")
	b.WriteString(escapeMarkdown(title))
	if body := strings.TrimSpace(stringField(item["body"])); body != "" {
		if utf8.RuneCountInString(body) > inboxBodyMaxRunes {
			body = string([]rune(body)[:inboxBodyMaxRunes]) + "…"
		}
		b.WriteString("\n\n")
		b.WriteString(escapeMarkdown(body))
	}
	if link := inboxIssueLink(item, appURL, slug); link != "" {
		b.WriteString("\n\n[")
		b.WriteString(msgInboxOpen)
		b.WriteString("](")
		b.WriteString(link)
		b.WriteString(")")
	}
	return b.String()
}

func inboxHeadline(item map[string]any) string {
	switch stringField(item["type"]) {
	case "status_changed":
		if label, ok := msgInboxStatus[stringField(item["issue_status"])]; ok {
			return label
		}
	case "mentioned":
		return msgInboxMentioned
	}
	return msgInboxComment
}

func inboxIssueLink(item map[string]any, appURL, slug string) string {
	issueID := stringField(item["issue_id"])
	if appURL == "" || slug == "" || issueID == "" {
		return ""
	}
	return appURL + "/" + url.PathEscape(slug) + "/issues/" + url.PathEscape(issueID)
}

// stringField reads a payload string that may arrive plain or behind a
// pointer (inbox items carry nullable columns as *string).
func stringField(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case *string:
		if s != nil {
			return *s
		}
	}
	return ""
}
