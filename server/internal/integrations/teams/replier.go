package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Teams OutboundReplier — the engine seam that answers a
// pipeline verdict (needs binding, agent offline, /issue outcome, …) back in
// the conversation the message came from.

// bindingMinter is the binding-token surface the replier needs.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, teamsUserID string) (BindingToken, error)
}

// OutboundReplier implements engine.OutboundReplier for Teams.
type OutboundReplier struct {
	binding     bindingMinter
	transport   *Transport
	appURL      string
	bindingPath string
	logger      *slog.Logger
}

// OutboundReplierConfig configures the replier.
type OutboundReplierConfig struct {
	Binding   bindingMinter
	Transport *Transport
	// AppURL is the Multica web app origin for the bind link
	// (MULTICA_APP_URL ?? FRONTEND_ORIGIN, same as the other adapters).
	AppURL      string
	BindingPath string // default "/teams/bind"
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	path := cfg.BindingPath
	if path == "" {
		path = "/teams/bind"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &OutboundReplier{
		binding:     cfg.Binding,
		transport:   cfg.Transport,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: path,
		logger:      logger,
	}
}

// Reply routes each outcome to its user-visible message. Errors are logged:
// the replier runs detached from the inbound path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	var (
		text string
		err  error
	)
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		err = r.sendBindingPrompt(ctx, inst, msg, res)
	case engine.OutcomeAgentOffline:
		text = msgAgentOffline
	case engine.OutcomeAgentArchived:
		text = msgAgentArchived
	case engine.OutcomeFreshPending:
		text = msgFreshPending
	case engine.OutcomeChatStarted:
		text = msgChatStarted
	case engine.OutcomeIssueUsage:
		text = msgIssueUsage
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			text = issueCreatedText(res)
			if res.IssueDuplicate {
				text = issueDuplicateText(res)
			}
		}
	case engine.OutcomeDropped:
		text = droppedReplyText(res, msg)
	}
	if text != "" {
		err = r.post(ctx, inst, msg, text)
	}
	if err != nil {
		r.logger.WarnContext(ctx, "teams replier: reply failed",
			"installation_id", util.UUIDToString(inst.ID), "outcome", res.Outcome, "error", err)
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	// A bind link posted where others can see it could be redeemed by the
	// wrong person and bind this sender's Teams identity to their account.
	// Only personal chats get a link.
	if msg.Source.ChatType == channel.ChatTypeGroup {
		return r.post(ctx, inst, msg, msgBindingGroupHint)
	}
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil || r.appURL == "" {
		return errors.New("binding service or app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	link := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	return r.post(ctx, inst, msg, fmt.Sprintf(msgBindingPrompt, link))
}

// post sends text back into the message's conversation.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	raw, err := decodeRaw(msg)
	if err != nil {
		return err
	}
	conn, _, err := r.transport.connectorFor(row.Config)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	conversationID := firstNonEmpty(raw.ConversationID, msg.Source.ChatID)
	if _, err := conn.SendMessage(ctx, raw.ServiceURL, conversationID, text, ""); err != nil {
		return fmt.Errorf("post teams reply: %w", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := issueResultIdentifier(res)
	if title := strings.TrimSpace(res.IssueTitle); title != "" {
		return fmt.Sprintf(msgIssueCreatedTitle, id, title)
	}
	return fmt.Sprintf(msgIssueCreatedFmt, id)
}

func issueDuplicateText(res engine.Result) string {
	id := issueResultIdentifier(res)
	if title := strings.TrimSpace(res.IssueTitle); title != "" {
		return fmt.Sprintf(msgIssueDupTitle, id, title)
	}
	return fmt.Sprintf(msgIssueDupFmt, id)
}

func issueResultIdentifier(res engine.Result) string {
	if res.IssueIdentifier != "" {
		return res.IssueIdentifier
	}
	if res.IssueNumber > 0 {
		return fmt.Sprintf("#%d", res.IssueNumber)
	}
	return util.UUIDToString(res.IssueID)
}

func isAddressedIssueCommand(msg channel.InboundMessage) bool {
	if !msg.AddressedToBot {
		return false
	}
	source := msg.CommandText
	if source == "" {
		source = msg.Text
	}
	_, ok := engine.ParseIssueCommand(source)
	return ok
}

func droppedReplyText(res engine.Result, msg channel.InboundMessage) string {
	switch res.DropReason {
	case engine.DropReasonNonWorkspaceMember:
		// Unlike the other adapters, a non-member is told on any addressed
		// message: Teams users reach the bot through their company directory,
		// so "why doesn't it answer me?" is the common case to explain.
		if msg.AddressedToBot || msg.Source.ChatType == channel.ChatTypeP2P {
			return msgNotMember
		}
	case engine.DropReasonRevokedInstallation:
		if isAddressedIssueCommand(msg) {
			return msgIssueDisabled
		}
	}
	return ""
}
