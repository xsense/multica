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

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// This file is the Teams ResolverSet: the platform seams the engine.Router
// runs the inbound pipeline through, built on the generic channel_* queries
// and the shared engine.ChatSession — no schema change.

// originType is left empty on purpose: issue.origin_type is guarded by a
// CHECK constraint upstream rewrites whenever it adds a channel, and a fork
// value there would break the next upgrade. Issues created with /issue from
// Teams still carry origin_id = the chat session.
const originType = ""

// ResolverSetConfig wires the Teams ResolverSet.
type ResolverSetConfig struct {
	Queries   *db.Queries
	Tx        engine.TxStarter
	Transport *Transport
	Replier   engine.OutboundReplier
	Typing    engine.TypingNotifier
	// Media ingests pictures and personal-chat files; nil = text only.
	Media engine.MediaResolver
	// AutoBindDomains enables binding a Teams user to the Multica account
	// with the same email on first contact, for addresses on these domains
	// only (lower-case, no "@"). Empty = off: everyone uses the bind link.
	// Only list domains your tenant has verified — the email comes from the
	// tenant directory, which tenant admins can edit.
	AutoBindDomains []string
	Logger          *slog.Logger
}

// NewResolverSet assembles the Teams ResolverSet. Media (media.go) carries
// pasted pictures and personal-chat files into object storage; files posted
// in channels / group chats need Graph consent and get a notice instead.
func NewResolverSet(cfg ResolverSetConfig) engine.ResolverSet {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return engine.ResolverSet{
		Installation: &installationResolver{q: cfg.Queries},
		Identity: &identityResolver{
			q:         cfg.Queries,
			transport: cfg.Transport,
			domains:   normalizeDomains(cfg.AutoBindDomains),
			logger:    logger,
		},
		Dedup: &deduper{q: cfg.Queries},
		Session: &sessionBinder{session: engine.NewChatSession(cfg.Queries, cfg.Tx, TypeTeams, engine.SessionTitles{
			Group:    "Teams channel",
			Direct:   "Teams chat",
			Fallback: "Teams chat",
		})},
		Audit:      &auditor{q: cfg.Queries},
		Media:      cfg.Media,
		Replier:    cfg.Replier,
		Typing:     cfg.Typing,
		OriginType: originType,
	}
}

var (
	_ engine.InstallationResolver = (*installationResolver)(nil)
	_ engine.IdentityResolver     = (*identityResolver)(nil)
	_ engine.Deduper              = (*deduper)(nil)
	_ engine.SessionBinder        = (*sessionBinder)(nil)
	_ engine.Auditor              = (*auditor)(nil)
)

// bindingConfig is the outbound routing persisted on the chat binding: what
// a decoupled reply needs to reach the conversation later.
type bindingConfig struct {
	ConversationID string `json:"conversation_id"`
	ServiceURL     string `json:"service_url"`
	TenantID       string `json:"tenant_id"`
}

// sessionRouting derives the session key, binding config and reply thread
// from one inbound message. The conversation id already isolates channel
// threads, so the key is always the conversation id.
func sessionRouting(msg channel.InboundMessage) (bindingKey string, config []byte, replyThread string) {
	raw, _ := decodeRaw(msg)
	cfg, _ := json.Marshal(bindingConfig{
		ConversationID: firstNonEmpty(raw.ConversationID, msg.Source.ChatID),
		ServiceURL:     raw.ServiceURL,
		TenantID:       raw.TenantID,
	})
	return msg.Source.ChatID, cfg, msg.Source.ThreadID
}

func decodeRaw(msg channel.InboundMessage) (teamsRawEvent, error) {
	var raw teamsRawEvent
	if len(msg.Raw) == 0 {
		return teamsRawEvent{}, errors.New("teams: inbound message Raw is empty")
	}
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		return teamsRawEvent{}, fmt.Errorf("decode teams inbound raw: %w", err)
	}
	return raw, nil
}

func nullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- installation routing ----

type installationResolver struct{ q *db.Queries }

func (r *installationResolver) ResolveInstallation(ctx context.Context, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	raw, err := decodeRaw(msg)
	if err != nil {
		return engine.ResolvedInstallation{}, err
	}
	inst, err := r.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeTeams),
		AppID:       raw.AppID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
		}
		return engine.ResolvedInstallation{}, err
	}
	return engine.ResolvedInstallation{
		ID:              inst.ID,
		WorkspaceID:     inst.WorkspaceID,
		AgentID:         inst.AgentID,
		InstallerUserID: inst.InstallerUserID,
		Active:          inst.Status == "active",
		Platform:        inst,
	}, nil
}

// ---- identity ----

// identityQueries is what identity resolution needs; *db.Queries satisfies it.
type identityQueries interface {
	GetChannelUserBindingByUserID(ctx context.Context, arg db.GetChannelUserBindingByUserIDParams) (db.ChannelUserBinding, error)
	GetMemberByUserAndWorkspace(ctx context.Context, arg db.GetMemberByUserAndWorkspaceParams) (db.Member, error)
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	CreateChannelUserBinding(ctx context.Context, arg db.CreateChannelUserBindingParams) (db.ChannelUserBinding, error)
}

type identityResolver struct {
	q         identityQueries
	transport *Transport
	domains   map[string]bool
	logger    *slog.Logger
}

func normalizeDomains(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d != "" {
			out[d] = true
		}
	}
	return out
}

// memberLookupTimeout bounds the Connector member lookup on the inbound path.
const memberLookupTimeout = 5 * time.Second

func (r *identityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	})
	switch {
	case err == nil:
		return r.requireMember(ctx, inst, binding.MulticaUserID)
	case !errors.Is(err, pgx.ErrNoRows):
		return engine.ResolvedIdentity{}, err
	}

	if len(r.domains) == 0 {
		return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
	}
	userID, ok := r.bindByEmail(ctx, inst, msg)
	if !ok {
		return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
	}
	return r.requireMember(ctx, inst, userID)
}

func (r *identityResolver) requireMember(ctx context.Context, inst engine.ResolvedInstallation, userID pgtype.UUID) (engine.ResolvedIdentity, error) {
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      userID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderNotMember
		}
		return engine.ResolvedIdentity{}, err
	}
	return engine.ResolvedIdentity{UserID: userID}, nil
}

// bindByEmail asks the Connector for the sender's email and, when it belongs
// to a member of the installation's workspace, records the binding.
//
// Trust: the webhook has proven the activity came from the Connector for
// this bot and that the conversation belongs to the installation's tenant;
// the member lookup is an authenticated Connector call for that conversation.
// The email is the tenant directory's value for the sender. That is only as
// trustworthy as the directory, which tenant admins (and guest invites) can
// set to any address, so it is accepted only when: the member is not a
// guest, and the address is on an operator-allowlisted domain (one the
// tenant has verified). Everyone else falls back to the one-time bind link.
func (r *identityResolver) bindByEmail(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (pgtype.UUID, bool) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok || r.transport == nil {
		return pgtype.UUID{}, false
	}
	raw, err := decodeRaw(msg)
	if err != nil || raw.FromID == "" || raw.ServiceURL == "" {
		return pgtype.UUID{}, false
	}
	conn, creds, err := r.transport.connectorFor(row.Config)
	if err != nil {
		r.logger.WarnContext(ctx, "teams identity: decode credentials failed", "error", err)
		return pgtype.UUID{}, false
	}
	lctx, cancel := context.WithTimeout(ctx, memberLookupTimeout)
	defer cancel()
	member, err := conn.GetMember(lctx, raw.ServiceURL, raw.ConversationID, raw.FromID)
	if err != nil {
		r.logger.WarnContext(ctx, "teams identity: member lookup failed", "error", err)
		return pgtype.UUID{}, false
	}
	if t, _ := normalizeGUID(member.TenantID); t != "" && t != creds.TenantID {
		return pgtype.UUID{}, false
	}
	if member.AADObjectID != "" && msg.Source.SenderID != "" && !strings.EqualFold(member.AADObjectID, msg.Source.SenderID) {
		// The lookup must describe the same person the binding is keyed by.
		return pgtype.UUID{}, false
	}
	if strings.EqualFold(member.UserRole, "guest") {
		return pgtype.UUID{}, false
	}
	email := memberEmail(member)
	if email == "" {
		return pgtype.UUID{}, false
	}
	if _, domain, _ := strings.Cut(email, "@"); !r.domains[domain] {
		return pgtype.UUID{}, false
	}
	user, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			r.logger.WarnContext(ctx, "teams identity: user lookup failed", "error", err)
		}
		return pgtype.UUID{}, false
	}
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      user.ID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		// Not a member: no binding, so the engine answers with the bind link
		// and the person can ask an admin for an invite.
		return pgtype.UUID{}, false
	}
	cfg, _ := json.Marshal(map[string]string{"bound_by": "email", "email": email})
	if _, err := r.q.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
		WorkspaceID:    inst.WorkspaceID,
		MulticaUserID:  user.ID,
		InstallationID: inst.ID,
		ChannelType:    string(TypeTeams),
		ChannelUserID:  msg.Source.SenderID,
		Config:         cfg,
	}); err != nil {
		// ErrNoRows: this Teams id is already bound to someone else. Leave it.
		if !errors.Is(err, pgx.ErrNoRows) {
			r.logger.WarnContext(ctx, "teams identity: create binding failed", "error", err)
		}
		return pgtype.UUID{}, false
	}
	r.logger.InfoContext(ctx, "teams identity: bound Teams user by email",
		"installation_id", util.UUIDToString(inst.ID), "user_id", util.UUIDToString(user.ID))
	return user.ID, true
}

// memberEmail picks the member's mailbox, lower-cased the way Multica stores
// user emails. userPrincipalName is used only when it looks like an address.
func memberEmail(m teamsMember) string {
	for _, v := range []string{m.Email, m.UserPrincipalName} {
		v = strings.ToLower(strings.TrimSpace(v))
		if strings.Count(v, "@") == 1 && !strings.HasPrefix(v, "@") && !strings.HasSuffix(v, "@") &&
			!strings.Contains(v, "#ext#") {
			return v
		}
	}
	return ""
}

// ---- dedup ----

type deduper struct{ q *db.Queries }

func (r *deduper) Claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, error) {
	claim, err := r.q.ClaimChannelInboundDedup(ctx, db.ClaimChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, engine.ErrDuplicate
		}
		return pgtype.UUID{}, err
	}
	return claim.ClaimToken, nil
}

func (r *deduper) Mark(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

func (r *deduper) Release(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.ReleaseChannelInboundDedup(ctx, db.ReleaseChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

// ---- session bind / append ----

// chatSession is the shared engine session surface, behind a narrow seam so
// the parameter mapping is testable without a database.
type chatSession interface {
	EnsureSession(ctx context.Context, in engine.EnsureSessionInput) (pgtype.UUID, error)
	StartSession(ctx context.Context, in engine.StartSessionInput) (engine.StartSessionResult, error)
	MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error
	AppendUserMessage(ctx context.Context, in engine.AppendInput) (engine.AppendResult, error)
	BindMediaRefs(ctx context.Context, in engine.BindMediaInput) error
}

type sessionBinder struct{ session chatSession }

func (r *sessionBinder) EnsureSession(ctx context.Context, p engine.EnsureSessionParams) (pgtype.UUID, error) {
	bindingKey, config, _ := sessionRouting(p.Message)
	return r.session.EnsureSession(ctx, engine.EnsureSessionInput{
		WorkspaceID:    p.Installation.WorkspaceID,
		AgentID:        p.Installation.AgentID,
		InstallationID: p.Installation.ID,
		Sender:         p.Sender,
		BindingKey:     bindingKey,
		BindingConfig:  config,
		ChatType:       p.Message.Source.ChatType,
	})
}

func (r *sessionBinder) StartSession(ctx context.Context, p engine.StartSessionParams) (engine.StartSessionResult, error) {
	bindingKey, config, replyThread := sessionRouting(p.Message)
	result, err := r.session.StartSession(ctx, engine.StartSessionInput{
		EnsureSessionInput: engine.EnsureSessionInput{
			WorkspaceID: p.Installation.WorkspaceID, AgentID: p.Installation.AgentID,
			InstallationID: p.Installation.ID, Sender: p.Creator,
			BindingKey: bindingKey, BindingConfig: config, ChatType: p.Message.Source.ChatType,
		},
		Initiator: p.Sender,
		Body:      p.Message.Text, CommandText: p.Message.CommandText, MessageID: p.Message.MessageID, ThreadID: replyThread,
		ClaimToken: p.ClaimToken, MediaPendingSeconds: p.MediaPendingSeconds,
		PersistMessage: p.PersistMessage, HistoryBoundaryPending: p.HistoryBoundaryPending,
		BeforeCommit: p.BeforeCommit,
	})
	return engine.StartSessionResult{
		SessionID: result.SessionID, BindingID: result.BindingID,
		RouteRevision: result.RouteRevision, Append: result.Append,
	}, err
}

func (r *sessionBinder) MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error {
	return r.session.MarkPendingFresh(ctx, sessionID, messageID)
}

func (r *sessionBinder) AppendMessage(ctx context.Context, p engine.AppendParams) (engine.AppendResult, error) {
	_, _, replyThread := sessionRouting(p.Message)
	commandText := p.Message.CommandText
	if commandText == "" {
		commandText = p.Message.Text
	}
	return r.session.AppendUserMessage(ctx, engine.AppendInput{
		SessionID:           p.SessionID,
		Sender:              p.Sender,
		InstallationID:      p.InstallationID,
		Body:                p.Message.Text,
		CommandText:         commandText,
		MessageID:           p.Message.MessageID,
		ThreadID:            replyThread,
		ClaimToken:          p.ClaimToken,
		MediaPendingSeconds: p.MediaPendingSeconds,
		ForceFresh:          p.Message.ForceFresh,
	})
}

func (r *sessionBinder) BindMedia(ctx context.Context, p engine.BindMediaParams) (engine.BindMediaResult, error) {
	in := engine.BindMediaInput{
		MessageID:   p.MessageID,
		SessionID:   p.SessionID,
		WorkspaceID: p.WorkspaceID,
		Sender:      p.Sender,
		MediaRefs:   p.MediaRefs,
	}
	if richer, ok := r.session.(interface {
		BindMediaRefsWithResult(context.Context, engine.BindMediaInput) (engine.BindMediaResult, error)
	}); ok {
		return richer.BindMediaRefsWithResult(ctx, in)
	}
	return engine.BindMediaResult{}, r.session.BindMediaRefs(ctx, in)
}

// ---- audit ----

type auditor struct{ q *db.Queries }

func (r *auditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
	raw, _ := decodeRaw(msg) // best-effort; a decode miss still audits
	return r.q.RecordChannelInboundDrop(ctx, db.RecordChannelInboundDropParams{
		ID:               dbid.NewV7(),
		ChannelType:      string(TypeTeams),
		EventType:        raw.EventType,
		DropReason:       string(reason),
		InstallationID:   instID,
		ChannelChatID:    nullText(msg.Source.ChatID),
		ChannelEventID:   nullText(msg.EventID),
		ChannelMessageID: nullText(msg.MessageID),
	})
}
