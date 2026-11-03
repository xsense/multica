package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Teams install backend, mirroring telegram/install.go: the
// workspace admin pastes an Azure Bot's App ID, tenant ID and client secret.
// The InstallService verifies the credentials live (by minting a Connector
// token), encrypts the secret at rest, and persists the installation.

var (
	ErrInstallationNotFound = errors.New("teams installation not found")
	// ErrInvalidAppID / ErrInvalidTenantID: not GUID-shaped.
	ErrInvalidAppID    = errors.New("teams: App ID must be a GUID like 00000000-0000-0000-0000-000000000000")
	ErrInvalidTenantID = errors.New("teams: tenant ID must be a GUID like 00000000-0000-0000-0000-000000000000")
	ErrMissingSecret   = errors.New("teams: client secret is required")
	// ErrCredentialsRejected: Entra refused the App ID / tenant / secret.
	ErrCredentialsRejected = errors.New("teams: Microsoft rejected these bot credentials")
	// ErrCredentialsUnverifiable: Entra could not be reached; nothing saved.
	ErrCredentialsUnverifiable = errors.New("teams: could not reach Microsoft to verify these credentials")
	// ErrBotOwnedByAnotherWorkspace / SameWorkspace / ArchivedAgent: the App
	// ID is already connected elsewhere.
	ErrBotOwnedByAnotherWorkspace = errors.New("teams: this bot is already connected to a different Multica workspace")
	ErrBotOwnedBySameWorkspace    = errors.New("teams: this bot is already connected to another agent in this workspace")
	ErrBotOwnedByArchivedAgent    = errors.New("teams: this bot is connected to an archived agent in this workspace")
)

type installQueries interface {
	WithTx(tx pgx.Tx) installQueries
	UpsertChannelInstallation(ctx context.Context, arg db.UpsertChannelInstallationParams) (db.ChannelInstallation, error)
	ReclaimDeadChannelInstallationByAppID(ctx context.Context, arg db.ReclaimDeadChannelInstallationByAppIDParams) (pgtype.UUID, error)
	GetChannelInstallationOwnerByAppID(ctx context.Context, arg db.GetChannelInstallationOwnerByAppIDParams) (db.GetChannelInstallationOwnerByAppIDRow, error)
	ListChannelInstallationsByWorkspace(ctx context.Context, arg db.ListChannelInstallationsByWorkspaceParams) ([]db.ChannelInstallation, error)
	GetChannelInstallationInWorkspace(ctx context.Context, arg db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error)
	SetChannelInstallationStatus(ctx context.Context, arg db.SetChannelInstallationStatusParams) error
}

type dbInstallQueries struct{ *db.Queries }

func (q dbInstallQueries) WithTx(tx pgx.Tx) installQueries {
	return dbInstallQueries{q.Queries.WithTx(tx)}
}

// InstallService owns at-rest encryption of the secret and the install
// transaction. The box MUST be non-nil.
type InstallService struct {
	box       *secretbox.Box
	q         installQueries
	tx        engine.TxStarter
	transport *Transport
	logger    *slog.Logger
}

// NewInstallService binds the service to queries, a tx starter and the box.
func NewInstallService(q *db.Queries, tx engine.TxStarter, box *secretbox.Box, transport *Transport, logger *slog.Logger) (*InstallService, error) {
	if q == nil || tx == nil || box == nil || transport == nil {
		return nil, errors.New("teams: InstallService requires queries, tx, box and transport")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &InstallService{box: box, q: dbInstallQueries{q}, tx: tx, transport: transport, logger: logger}, nil
}

// RegisterParams are the inputs for a bot install.
type RegisterParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
	AppID       string
	TenantID    string
	AppSecret   string
	BotName     string
}

// Register verifies and persists a Teams bot for an agent.
func (s *InstallService) Register(ctx context.Context, p RegisterParams) (db.ChannelInstallation, error) {
	appID, ok := normalizeGUID(p.AppID)
	if !ok {
		return db.ChannelInstallation{}, ErrInvalidAppID
	}
	tenantID, ok := normalizeGUID(p.TenantID)
	if !ok {
		return db.ChannelInstallation{}, ErrInvalidTenantID
	}
	secret := strings.TrimSpace(p.AppSecret)
	if secret == "" {
		return db.ChannelInstallation{}, ErrMissingSecret
	}
	creds := credentials{AppID: appID, TenantID: tenantID, AppSecret: secret}
	if _, err := s.transport.tokens.mint(ctx, creds); err != nil {
		if errors.Is(err, ErrTokenRejected) {
			return db.ChannelInstallation{}, fmt.Errorf("%w: %v", ErrCredentialsRejected, err)
		}
		return db.ChannelInstallation{}, fmt.Errorf("%w: %v", ErrCredentialsUnverifiable, err)
	}

	sealed, err := s.box.Seal([]byte(secret))
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encrypt teams secret: %w", err)
	}
	botName := strings.TrimSpace(p.BotName)
	if len([]rune(botName)) > 30 {
		botName = string([]rune(botName)[:30])
	}
	cfgJSON, err := json.Marshal(installConfig{
		AppID:              appID,
		TenantID:           tenantID,
		BotName:            botName,
		AppSecretEncrypted: base64.StdEncoding.EncodeToString(sealed),
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode teams installation config: %w", err)
	}
	return s.persist(ctx, p, appID, cfgJSON)
}

const pgUniqueViolation = "23505"

func (s *InstallService) persist(ctx context.Context, p RegisterParams, appID string, cfgJSON []byte) (db.ChannelInstallation, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("begin install tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if _, err := qtx.ReclaimDeadChannelInstallationByAppID(ctx, db.ReclaimDeadChannelInstallationByAppIDParams{
		ChannelType: string(TypeTeams),
		AppID:       appID,
		WorkspaceID: p.WorkspaceID,
		AgentID:     p.AgentID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.ChannelInstallation{}, fmt.Errorf("reclaim dead teams installation: %w", err)
	}
	inst, err := qtx.UpsertChannelInstallation(ctx, db.UpsertChannelInstallationParams{
		WorkspaceID:     p.WorkspaceID,
		AgentID:         p.AgentID,
		ChannelType:     string(TypeTeams),
		Config:          cfgJSON,
		InstallerUserID: p.InitiatorID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return db.ChannelInstallation{}, s.ownerConflict(ctx, p.WorkspaceID, appID)
		}
		return db.ChannelInstallation{}, fmt.Errorf("upsert teams installation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("commit teams install: %w", err)
	}
	return inst, nil
}

func (s *InstallService) ownerConflict(ctx context.Context, wsID pgtype.UUID, appID string) error {
	owner, err := s.q.GetChannelInstallationOwnerByAppID(ctx, db.GetChannelInstallationOwnerByAppIDParams{
		ChannelType: string(TypeTeams),
		AppID:       appID,
	})
	if err != nil {
		return ErrBotOwnedByAnotherWorkspace
	}
	switch {
	case owner.WorkspaceID != wsID:
		return ErrBotOwnedByAnotherWorkspace
	case owner.AgentArchivedAt.Valid:
		return ErrBotOwnedByArchivedAgent
	default:
		return ErrBotOwnedBySameWorkspace
	}
}

// ListByWorkspace returns every Teams installation in the workspace.
func (s *InstallService) ListByWorkspace(ctx context.Context, wsID pgtype.UUID) ([]db.ChannelInstallation, error) {
	return s.q.ListChannelInstallationsByWorkspace(ctx, db.ListChannelInstallationsByWorkspaceParams{
		WorkspaceID: wsID,
		ChannelType: string(TypeTeams),
	})
}

// GetInWorkspace is the workspace-scoped lookup.
func (s *InstallService) GetInWorkspace(ctx context.Context, id, wsID pgtype.UUID) (db.ChannelInstallation, error) {
	inst, err := s.q.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID:          id,
		WorkspaceID: wsID,
		ChannelType: string(TypeTeams),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChannelInstallation{}, ErrInstallationNotFound
		}
		return db.ChannelInstallation{}, err
	}
	return inst, nil
}

// Revoke flips status to 'revoked'; the row is kept for audit.
func (s *InstallService) Revoke(ctx context.Context, id pgtype.UUID) error {
	return s.q.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{
		ID:     id,
		Status: "revoked",
	})
}
