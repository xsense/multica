package teams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Teams messaging endpoint: the Bot Framework Connector POSTs
// every activity for every Teams bot connected to this deployment to one URL
// (POST /api/teams/messages). The handler identifies the bot from the
// activity, proves the request came from the Connector for that bot, checks
// the tenant, and hands message activities to the shared engine.Router.

const (
	// maxActivityBytes caps one inbound activity. Teams messages top out
	// around 28 KB of text; the rest is envelope.
	maxActivityBytes = 512 << 10
	// handleTimeout bounds the synchronous Router call. The Connector waits
	// ~15s before treating the delivery as failed and retrying.
	handleTimeout = 12 * time.Second
)

// webhookQueries is the slice of generated queries the endpoint needs.
type webhookQueries interface {
	GetChannelInstallationByAppID(ctx context.Context, arg db.GetChannelInstallationByAppIDParams) (db.ChannelInstallation, error)
}

// Webhook serves POST /api/teams/messages.
type Webhook struct {
	q         webhookQueries
	transport *Transport
	auth      *Authenticator
	handle    channel.InboundHandler
	logger    *slog.Logger
	// spawn runs best-effort notices off the request path (tests run inline).
	spawn func(func())
}

// WebhookConfig wires the endpoint.
type WebhookConfig struct {
	Queries   *db.Queries
	Transport *Transport
	// Auth defaults to the public Bot Framework metadata.
	Auth *Authenticator
	// Handle is the engine inbound entry point (engine.Router.Handle).
	Handle channel.InboundHandler
	Logger *slog.Logger
}

// NewWebhook builds the endpoint handler.
func NewWebhook(cfg WebhookConfig) *Webhook {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	auth := cfg.Auth
	if auth == nil {
		auth = NewAuthenticator("", nil)
	}
	return &Webhook{
		q:         cfg.Queries,
		transport: cfg.Transport,
		auth:      auth,
		handle:    cfg.Handle,
		logger:    logger,
		spawn:     func(f func()) { go f() },
	}
}

// ServeHTTP implements the messaging endpoint. Status codes follow the Bot
// Framework contract: 2xx = delivered (including deliberate drops), 401/403 =
// authentication failure (never retried), 5xx = try again later.
func (w *Webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxActivityBytes+1))
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > maxActivityBytes {
		rw.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var act Activity
	if err := json.Unmarshal(body, &act); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	appID := act.botAppID()
	if appID == "" {
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	inst, err := w.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeTeams),
		AppID:       appID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown bot: indistinguishable from a forged request.
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.logger.ErrorContext(ctx, "teams webhook: installation lookup failed", "error", err)
		rw.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	conn, creds, err := w.transport.connectorFor(inst.Config)
	if err != nil {
		w.logger.ErrorContext(ctx, "teams webhook: decode installation credentials failed", "app_id", appID, "error", err)
		rw.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	if err := w.auth.Verify(ctx, r.Header.Get("Authorization"), creds.AppID, act.ChannelID, act.ServiceURL); err != nil {
		w.logger.WarnContext(ctx, "teams webhook: rejected unauthenticated request", "app_id", appID, "error", err)
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	if act.ChannelID != "msteams" {
		// Web Chat, Emulator and other Bot Framework channels are not served.
		rw.WriteHeader(http.StatusOK)
		return
	}
	tenantID := act.tenantID()
	if tenantID == "" || tenantID != creds.TenantID {
		// A single-tenant bot can be added in another tenant only by a
		// misconfiguration; never let that tenant's users reach the agent.
		w.logger.WarnContext(ctx, "teams webhook: activity from a foreign tenant ignored",
			"app_id", appID, "tenant_id", tenantID)
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	if _, err := validServiceURL(act.ServiceURL); err != nil {
		w.logger.WarnContext(ctx, "teams webhook: untrusted serviceUrl", "app_id", appID, "error", err)
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	switch act.Type {
	case "message":
		w.handleMessage(ctx, rw, act, appID, tenantID, conn, inst.Status == "active")
	case "conversationUpdate":
		w.handleConversationUpdate(act, conn, inst.Status == "active")
		rw.WriteHeader(http.StatusOK)
	case "invoke":
		// Card actions, message extensions… none are offered. Answer the
		// invoke so Teams does not show an error to the user.
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"status":200}`))
	default:
		// typing, messageReaction, installationUpdate, …: nothing to do.
		rw.WriteHeader(http.StatusOK)
	}
}

func (w *Webhook) handleMessage(ctx context.Context, rw http.ResponseWriter, act Activity, appID, tenantID string, conn *connector, active bool) {
	msg, ok := inboundFromActivity(act, appID, tenantID)
	if !ok {
		rw.WriteHeader(http.StatusOK)
		return
	}
	if msg.Source.ChatType == channel.ChatTypeGroup && !msg.AddressedToBot {
		// Without RSC permissions Teams only delivers @-mentions in channels;
		// in group chats with RSC it delivers everything. Unaddressed chatter
		// is never a turn.
		rw.WriteHeader(http.StatusOK)
		return
	}
	if msg.Type == channel.MsgTypeUnknown {
		if active {
			w.notify(act, conn, msgUnsupportedType)
		}
		rw.WriteHeader(http.StatusOK)
		return
	}

	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
	defer cancel()
	if err := w.handle(hctx, msg); err != nil {
		// Infrastructure failure: let the Connector redeliver. The engine's
		// (installation, message_id) dedup absorbs a delivery that did land.
		w.logger.ErrorContext(ctx, "teams webhook: engine rejected message", "app_id", appID, "error", err)
		if isAddressedIssueCommand(msg) {
			w.notify(act, conn, msgIssueFailed)
		}
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

// handleConversationUpdate greets the user when the bot is added to a
// personal chat. Channel / group installs stay quiet: a bot announcing itself
// in a busy channel is noise.
func (w *Webhook) handleConversationUpdate(act Activity, conn *connector, active bool) {
	if !active || act.Conversation.ConversationType != "personal" {
		return
	}
	for _, m := range act.MembersAdded {
		if m.ID == act.Recipient.ID {
			w.notify(act, conn, msgWelcome)
			return
		}
	}
}

// notify posts a best-effort notice into the activity's conversation.
func (w *Webhook) notify(act Activity, conn *connector, text string) {
	// The conversation id of a channel message already pins its thread, so a
	// plain send lands in the right place everywhere.
	serviceURL, conversationID := act.ServiceURL, act.Conversation.ID
	w.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), detachedTimeout)
		defer cancel()
		if _, err := conn.SendMessage(ctx, serviceURL, conversationID, text, ""); err != nil {
			w.logger.Warn("teams webhook: notice failed", "error", err)
		}
	})
}
