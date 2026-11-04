package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Teams messaging endpoint: the Bot Framework Connector POSTs
// every activity for every Teams bot connected to this deployment to one URL
// (POST /api/webhooks/teams). The handler identifies the bot from the
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

// execer runs one statement; *pgxpool.Pool satisfies it.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// refreshServiceURLSQL keeps the reply route current. The engine writes a
// binding's config once, when the session is created, but Teams moves
// conversations between Connector endpoints over time (regional and
// per-tenant serviceUrl paths). Every verified inbound activity carries the
// endpoint the Connector currently accepts replies on; store it before the
// engine runs so the task's delivery snapshot picks it up.
const refreshServiceURLSQL = `UPDATE channel_chat_session_binding
SET config = config || jsonb_build_object('service_url', $3::text)
WHERE installation_id = $1 AND channel_type = 'teams' AND channel_chat_id = $2
  AND config->>'service_url' IS DISTINCT FROM $3::text`

// Webhook serves POST /api/webhooks/teams.
type Webhook struct {
	q  webhookQueries
	db execer
	// acceptsMedia: object storage + a media resolver are wired.
	acceptsMedia bool
	transport    *Transport
	auth         *Authenticator
	handle       channel.InboundHandler
	logger       *slog.Logger
	// spawn runs best-effort notices off the request path (tests run inline).
	spawn func(func())
}

// WebhookConfig wires the endpoint.
type WebhookConfig struct {
	Queries *db.Queries
	// DB refreshes stored reply routes (normally the pgx pool). Optional.
	DB        execer
	Transport *Transport
	// AcceptsMedia must match whether the ResolverSet carries a media
	// resolver (both follow object storage being configured).
	AcceptsMedia bool
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
		q:            cfg.Queries,
		db:           cfg.DB,
		acceptsMedia: cfg.AcceptsMedia,
		transport:    cfg.Transport,
		auth:         auth,
		handle:       cfg.Handle,
		logger:       logger,
		spawn:        func(f func()) { go f() },
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

	ctx := r.Context()
	// Route to an installation. The recipient ("28:<app id>") is the usual
	// key, but its shape varies by channel (Web Chat uses the bot handle), so
	// the token's audience is the fallback. Neither is trusted here: Verify
	// below proves the token was minted for the installation's own App ID.
	// A GUID-shaped recipient is authoritative; the audience is consulted only
	// when the recipient is not one (so an activity addressed to another bot
	// is never re-routed to this one).
	candidates := routingCandidates(act.botAppID())
	if len(candidates) == 0 {
		candidates = routingCandidates(unverifiedAudience(r.Header.Get("Authorization")))
	}
	if len(candidates) == 0 {
		w.logger.InfoContext(ctx, "teams webhook: no bot id in request",
			"channel_id", act.ChannelID, "recipient_id", act.Recipient.ID, "type", act.Type)
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	var (
		inst  db.ChannelInstallation
		appID string
		found bool
	)
	for _, id := range candidates {
		row, err := w.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
			ChannelType: string(TypeTeams),
			AppID:       id,
		})
		if err == nil {
			inst, appID, found = row, id, true
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			w.logger.ErrorContext(ctx, "teams webhook: installation lookup failed", "error", err)
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	if !found {
		// Unknown bot: indistinguishable from a forged request.
		w.logger.InfoContext(ctx, "teams webhook: no Teams installation for this bot",
			"candidates", candidates, "channel_id", act.ChannelID, "recipient_id", act.Recipient.ID)
		rw.WriteHeader(http.StatusUnauthorized)
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
		w.logger.InfoContext(ctx, "teams webhook: verified activity from a non-Teams channel ignored",
			"app_id", appID, "channel_id", act.ChannelID)
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
		w.handleMessage(ctx, rw, act, appID, tenantID, conn, inst.ID, inst.Status == "active")
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

func (w *Webhook) handleMessage(ctx context.Context, rw http.ResponseWriter, act Activity, appID, tenantID string, conn *connector, instID pgtype.UUID, active bool) {
	msg, ok := inboundFromActivityWithMedia(act, appID, tenantID, w.acceptsMedia)
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
			w.notify(act, conn, w.unsupportedNotice())
		}
		rw.WriteHeader(http.StatusOK)
		return
	}

	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
	defer cancel()
	if w.db != nil {
		if _, err := w.db.Exec(hctx, refreshServiceURLSQL, instID, msg.Source.ChatID, act.ServiceURL); err != nil {
			w.logger.WarnContext(ctx, "teams webhook: refresh reply route failed", "error", err)
		}
	}
	if err := w.handle(hctx, msg); err != nil {
		// Infrastructure failure: let the Connector redeliver. The engine's
		// (installation, message_id) dedup absorbs a delivery that did land.
		// No user notice here: the Connector retries a 5xx, and each retry
		// would post it again. A retry that also fails is logged.
		w.logger.ErrorContext(ctx, "teams webhook: engine rejected message", "app_id", appID, "error", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Only after a successful hand-off: a 5xx is redelivered, and a notice
	// sent before it would be posted again on every retry.
	if raw, _ := decodeRaw(msg); raw.UnsupportedFile && active {
		w.notify(act, conn, w.unsupportedNotice())
	}
	rw.WriteHeader(http.StatusOK)
}

// unsupportedNotice explains why a file was left behind.
func (w *Webhook) unsupportedNotice() string {
	if !w.acceptsMedia {
		return msgFilesDisabled
	}
	return msgUnsupportedType
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

// routingCandidates returns the distinct, GUID-shaped App ID candidates.
func routingCandidates(ids ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		norm, ok := normalizeGUID(id)
		if ok && !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	return out
}

// unverifiedAudience reads the aud claim of a bearer JWT WITHOUT checking
// it. Used only to pick which installation's App ID to verify the token
// against; Verify then enforces aud == that App ID with a valid signature.
func unverifiedAudience(authHeader string) string {
	scheme, raw, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 || len(parts[1]) > 8192 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(payload, &claims) != nil || len(claims.Aud) == 0 {
		return ""
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(claims.Aud, &many) == nil && len(many) > 0 {
		return many[0]
	}
	return ""
}
