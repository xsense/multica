package teams

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Teams shows "… is typing" for only a few seconds per typing activity, while
// an agent run takes minutes. The manager re-sends the indicator on a ticker
// from ingest until the run ends (chat done / failed / cancelled), the engine
// settles the session without a run, or a hard cap passes.

const (
	typingInterval = 3 * time.Second
	typingMaxSpan  = 15 * time.Minute
)

// TypingManager implements engine.TypingNotifier for Teams.
type TypingManager struct {
	transport *Transport
	logger    *slog.Logger

	mu     sync.Mutex
	active map[string]context.CancelFunc // chat_session_id → stop
}

var _ engine.TypingNotifier = (*TypingManager)(nil)

// NewTypingManager builds the manager.
func NewTypingManager(transport *Transport, logger *slog.Logger) *TypingManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &TypingManager{transport: transport, logger: logger, active: make(map[string]context.CancelFunc)}
}

// Register clears the indicator however the run ends.
func (m *TypingManager) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, m.handleEnd)
	bus.Subscribe(protocol.EventTaskFailed, m.handleEnd)
	bus.Subscribe(protocol.EventTaskCancelled, m.handleEnd)
}

func (m *TypingManager) handleEnd(e events.Event) {
	if e.ChatSessionID != "" {
		m.stop(e.ChatSessionID)
	}
}

// OnIngested starts (or keeps) the indicator for the message's session.
func (m *TypingManager) OnIngested(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, sessionID pgtype.UUID, _ pgtype.UUID) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok || !sessionID.Valid {
		return
	}
	raw, err := decodeRaw(msg)
	if err != nil || raw.ServiceURL == "" {
		return
	}
	conn, _, err := m.transport.connectorFor(row.Config)
	if err != nil {
		m.logger.WarnContext(ctx, "teams typing: decode credentials failed", "error", err)
		return
	}
	key := util.UUIDToString(sessionID)
	conversationID := firstNonEmpty(raw.ConversationID, msg.Source.ChatID)

	m.mu.Lock()
	if _, running := m.active[key]; running {
		m.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithTimeout(context.Background(), typingMaxSpan)
	m.active[key] = cancel
	m.mu.Unlock()

	go func() {
		defer m.stop(key)
		t := time.NewTicker(typingInterval)
		defer t.Stop()
		for {
			cctx, ccancel := context.WithTimeout(runCtx, 5*time.Second)
			err := conn.SendTyping(cctx, raw.ServiceURL, conversationID)
			ccancel()
			if err != nil && runCtx.Err() == nil {
				m.logger.Debug("teams typing: send failed", "error", err)
				if !isRetryable(err) {
					return
				}
			}
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// OnSettled clears the indicator when the engine decided no run will happen.
func (m *TypingManager) OnSettled(_ context.Context, sessionID pgtype.UUID, _ engine.TypingSettlement) {
	if sessionID.Valid {
		m.stop(util.UUIDToString(sessionID))
	}
}

func (m *TypingManager) stop(key string) {
	m.mu.Lock()
	cancel, ok := m.active[key]
	delete(m.active, key)
	m.mu.Unlock()
	if ok {
		cancel()
	}
}
