package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file delivers an agent's finished chat reply back to Teams — the
// outbound half of the round trip, shaped like the Slack adapter's: on
// EventChatDone it finds the Teams binding of the finished task and posts the
// answer into the originating conversation. A failed run gets a short notice
// so the user is not left staring at a typing indicator that just stopped.
//
// Streaming partial output by editing a placeholder (as the Telegram adapter
// does) is deliberately left out of v1: Teams throttles activity updates per
// conversation and shows an "Edited" label on every edit.

// outboundQueries is what the outbound subscriber needs; *db.Queries
// satisfies it.
type outboundQueries interface {
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	TaskHasChannelIngestedMessages(ctx context.Context, taskID pgtype.UUID) (bool, error)
	GetChannelTaskDelivery(ctx context.Context, taskID pgtype.UUID) (db.ChannelTaskDelivery, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
	SetChatMessageChannelOutboundProvenanceByTask(ctx context.Context, arg db.SetChatMessageChannelOutboundProvenanceByTaskParams) (int64, error)
	RecordChannelOutboundMessage(ctx context.Context, arg db.RecordChannelOutboundMessageParams) error
	ListAttachmentsByChatMessage(ctx context.Context, arg db.ListAttachmentsByChatMessageParams) ([]db.Attachment, error)
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
}

// Outbound is the bus subscriber that posts replies.
type Outbound struct {
	q         outboundQueries
	transport *Transport
	logger    *slog.Logger
	// spawn runs a delivery off the synchronous bus (tests run inline).
	spawn func(func())
	// objects is set by EnableFileDelivery; nil = text replies only.
	objects objectStore
	// appURL is the web app origin, for "open in Multica" file links.
	appURL string
}

// EnableFileDelivery turns on delivery of files the agent attached to its
// reply: pictures inline, other files as a link to the chat in Multica. Call
// at boot, before Register, only when object storage is configured.
func (o *Outbound) EnableFileDelivery(objects objectStore, appURL string) {
	o.objects = objects
	o.appURL = strings.TrimRight(appURL, "/")
}

// NewOutbound builds the subscriber.
func NewOutbound(q outboundQueries, transport *Transport, logger *slog.Logger) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	return &Outbound{q: q, transport: transport, logger: logger, spawn: func(f func()) { go f() }}
}

// Register subscribes to the completion and failure events.
func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, o.handleChatDone)
	bus.Subscribe(protocol.EventTaskFailed, o.handleTaskFailed)
}

// deliveryTimeout bounds one reply delivery, including a retry.
const deliveryTimeout = 60 * time.Second

func (o *Outbound) handleChatDone(e events.Event) {
	content := chatDoneContent(e.Payload)
	if strings.TrimSpace(content) == "" && (o.objects == nil || chatDoneMessageID(e.Payload) == "") {
		return // nothing to say and no files that could be bound
	}
	// Bus delivery is synchronous; never hold the publisher on a Teams call.
	o.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
		defer cancel()
		if err := o.deliver(ctx, e, content, "task_reply", true); err != nil {
			o.logger.WarnContext(ctx, "teams outbound: reply delivery failed",
				"error", err, "chat_session_id", e.ChatSessionID, "task_id", e.TaskID)
		}
	})
}

func (o *Outbound) handleTaskFailed(e events.Event) {
	if taskFailureRetryPending(e.Payload) {
		// The platform runs this turn again under a new task id; its answer
		// is still coming.
		return
	}
	o.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
		defer cancel()
		if err := o.deliver(ctx, e, msgTaskFailed, "failure_notice", false); err != nil {
			o.logger.WarnContext(ctx, "teams outbound: failure notice failed",
				"error", err, "task_id", e.TaskID)
		}
	})
}

// deliver posts text for the event's task when that task belongs to a Teams
// conversation. recordProvenance links the posted message ids to the
// assistant chat_message (only meaningful for the real answer).
func (o *Outbound) deliver(ctx context.Context, e events.Event, text, kind string, recordProvenance bool) (err error) {
	taskID, ok := eventTaskID(e)
	if !ok {
		return nil
	}
	delivery, err := o.q.GetChannelTaskDelivery(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // web chat, issue run, or another channel
		}
		return fmt.Errorf("lookup task delivery: %w", err)
	}
	if delivery.ChannelType != string(TypeTeams) {
		return nil
	}
	// A web/mobile turn can reuse a session that started in Teams; its answer
	// belongs only in Multica. Fail closed when the origin is unknown.
	task, err := o.q.GetAgentTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load agent task: %w", err)
	}
	fromChannel, err := engine.TaskInputIsChannelIngested(ctx, o.q, task)
	if err != nil {
		return fmt.Errorf("classify task input origin: %w", err)
	}
	if !fromChannel {
		return nil
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          delivery.InstallationID,
		ChannelType: string(TypeTeams),
	})
	if err != nil {
		return fmt.Errorf("load teams installation: %w", err)
	}
	if inst.Status != "active" {
		return nil // disconnected between trigger and reply
	}
	conn, _, err := o.transport.connectorFor(inst.Config)
	if err != nil {
		return fmt.Errorf("decode teams credentials: %w", err)
	}
	target := targetFromDelivery(delivery)
	if target.ServiceURL == "" {
		return errors.New("teams binding has no serviceUrl")
	}

	defer func() {
		// Files follow the reply text; if the text did not go out, sending
		// pictures on their own would read as an answer without words.
		if kind == "task_reply" && err == nil {
			o.deliverAttachments(ctx, conn, target, e)
		}
	}()
	var ids []string
	for _, chunk := range chunkMarkdown(text, maxMessageChars) {
		id, err := sendWithRetry(ctx, func(ctx context.Context) (string, error) {
			return conn.SendMessage(ctx, target.ServiceURL, target.ConversationID, chunk, "")
		})
		if err != nil {
			return fmt.Errorf("post teams reply: %w", err)
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	if !recordProvenance || len(ids) == 0 {
		return nil
	}
	rows, err := o.q.SetChatMessageChannelOutboundProvenanceByTask(ctx, db.SetChatMessageChannelOutboundProvenanceByTaskParams{
		ChannelType:    pgtype.Text{String: string(TypeTeams), Valid: true},
		InstallationID: delivery.InstallationID,
		ChannelChatID:  pgtype.Text{String: delivery.ChannelChatID, Valid: true},
		MessageIds:     ids,
		TaskID:         taskID,
	})
	if err != nil {
		return fmt.Errorf("record reply provenance: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("record reply provenance: updated %d assistant rows, want 1 (task %s)", rows, util.UUIDToString(taskID))
	}
	for _, id := range ids {
		if err := o.q.RecordChannelOutboundMessage(ctx, db.RecordChannelOutboundMessageParams{
			OutboundInstallationID: delivery.InstallationID,
			OutboundChannelType:    delivery.ChannelType,
			OutboundMessageID:      id,
			OutboundBindingID:      delivery.BindingID,
			OutboundRouteRevision:  delivery.RouteRevision,
			OutboundTaskID:         taskID,
			OutboundKind:           kind,
		}); err != nil {
			return fmt.Errorf("record outbound message: %w", err)
		}
	}
	return nil
}

// replyTarget is where a decoupled reply goes.
type replyTarget struct {
	ConversationID string
	ServiceURL     string
}

func targetFromDelivery(d db.ChannelTaskDelivery) replyTarget {
	t := replyTarget{ConversationID: d.ChannelChatID}
	if len(d.Config) > 0 {
		var cfg bindingConfig
		if json.Unmarshal(d.Config, &cfg) == nil {
			if cfg.ConversationID != "" {
				t.ConversationID = cfg.ConversationID
			}
			t.ServiceURL = cfg.ServiceURL
		}
	}
	return t
}

// sendWithRetry makes one retry on throttling / server errors. A definite 4xx
// is not retried; a transport error after the request was sent might have
// landed, but a duplicate answer is better than none.
func sendWithRetry(ctx context.Context, send func(context.Context) (string, error)) (string, error) {
	id, err := send(ctx)
	if err == nil || !isRetryable(err) {
		return id, err
	}
	select {
	case <-ctx.Done():
		return "", err
	case <-time.After(2 * time.Second):
	}
	return send(ctx)
}

func eventTaskID(e events.Event) (pgtype.UUID, bool) {
	raw := e.TaskID
	if raw == "" {
		switch p := e.Payload.(type) {
		case protocol.ChatDonePayload:
			raw = p.TaskID
		case map[string]any:
			raw, _ = p["task_id"].(string)
		}
	}
	id, err := util.ParseUUID(raw)
	return id, err == nil && id.Valid
}

func chatDoneContent(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}

func taskFailureRetryPending(payload any) bool {
	fields, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	retryPending, _ := fields["retry_pending"].(bool)
	return retryPending
}
