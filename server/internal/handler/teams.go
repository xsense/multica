package handler

// Microsoft Teams integration endpoints (fork addition, see
// server/internal/integrations/teams). Shapes mirror the Telegram handlers.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/teams"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TeamsInstallationResponse is the wire shape for a Teams installation. The
// encrypted client secret is never included.
type TeamsInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	AppID           string `json:"app_id"`
	TenantID        string `json:"tenant_id"`
	BotName         string `json:"bot_name"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func teamsInstallationToResponse(row db.ChannelInstallation) TeamsInstallationResponse {
	info := teams.DecodePublicConfig(row.Config)
	return TeamsInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		AppID:           info.AppID,
		TenantID:        info.TenantID,
		BotName:         info.BotName,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ListTeamsInstallations (GET /api/workspaces/{id}/teams/installations) is
// member-visible. messaging_endpoint is the URL the admin pastes into the
// Azure Bot's configuration.
func (h *Handler) ListTeamsInstallations(w http.ResponseWriter, r *http.Request) {
	if h.TeamsInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":      []TeamsInstallationResponse{},
			"configured":         false,
			"install_supported":  false,
			"messaging_endpoint": "",
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.TeamsInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list teams installations")
		return
	}
	out := make([]TeamsInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, teamsInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":      out,
		"configured":         true,
		"install_supported":  true,
		"messaging_endpoint": h.TeamsMessagingEndpoint,
	})
}

// RegisterTeamsRequest is the body for a bot install.
type RegisterTeamsRequest struct {
	AppID     string `json:"app_id"`
	TenantID  string `json:"tenant_id"`
	AppSecret string `json:"app_secret"`
	BotName   string `json:"bot_name"`
}

// RegisterTeamsBot (POST /api/workspaces/{id}/teams/install?agent_id=…)
// connects an Azure Bot to an agent. Admin-only at the router.
func (h *Handler) RegisterTeamsBot(w http.ResponseWriter, r *http.Request) {
	if h.TeamsInstall == nil {
		writeFeatureDisabled(w, "teams_not_configured", "teams integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterTeamsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	botName := strings.TrimSpace(body.BotName)
	if botName == "" {
		botName = agent.Name
	}
	row, err := h.TeamsInstall.Register(r.Context(), teams.RegisterParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: initiatorUUID,
		AppID:       body.AppID,
		TenantID:    body.TenantID,
		AppSecret:   body.AppSecret,
		BotName:     botName,
	})
	if err != nil {
		switch {
		case errors.Is(err, teams.ErrInvalidAppID), errors.Is(err, teams.ErrInvalidTenantID), errors.Is(err, teams.ErrMissingSecret):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, teams.ErrCredentialsRejected):
			writeError(w, http.StatusBadRequest, "Microsoft rejected these credentials — check the App ID, tenant ID and client secret (the secret VALUE, not its ID) and try again")
		case errors.Is(err, teams.ErrCredentialsUnverifiable):
			writeError(w, http.StatusServiceUnavailable, "could not reach Microsoft to verify this bot — check the server network and try again; nothing was saved")
		case errors.Is(err, teams.ErrBotOwnedBySameWorkspace):
			writeError(w, http.StatusConflict, "this Teams bot is already connected to another agent in this workspace — disconnect it there first")
		case errors.Is(err, teams.ErrBotOwnedByArchivedAgent):
			writeError(w, http.StatusConflict, "this Teams bot is connected to an archived agent — restore that agent or disconnect its bot first")
		case errors.Is(err, teams.ErrBotOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this Teams bot is already connected to a different Multica workspace")
		default:
			writeError(w, http.StatusInternalServerError, "could not save this Teams bot; nothing was saved")
		}
		return
	}
	h.publish(protocol.EventTeamsInstallationCreated, uuidToString(row.WorkspaceID), "user", userID, map[string]any{
		"id": uuidToString(row.ID),
	})
	writeJSON(w, http.StatusOK, teamsInstallationToResponse(row))
}

// RevokeTeamsInstallation (DELETE /api/workspaces/{id}/teams/installations/{installationId}).
func (h *Handler) RevokeTeamsInstallation(w http.ResponseWriter, r *http.Request) {
	if h.TeamsInstall == nil {
		writeFeatureDisabled(w, "teams_not_configured", "teams integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	if _, err := h.TeamsInstall.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, teams.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "teams installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.TeamsInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventTeamsInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// DownloadTeamsAppPackage (GET /api/workspaces/{id}/teams/installations/{installationId}/app-package)
// returns the zipped Teams app (manifest + icons) to upload in the Teams
// admin center. Admin-only at the router.
func (h *Handler) DownloadTeamsAppPackage(w http.ResponseWriter, r *http.Request) {
	if h.TeamsInstall == nil {
		writeFeatureDisabled(w, "teams_not_configured", "teams integration not configured")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	row, err := h.TeamsInstall.GetInWorkspace(r.Context(), instUUID, wsUUID)
	if err != nil {
		if errors.Is(err, teams.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "teams installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	info := teams.DecodePublicConfig(row.Config)
	agentName := ""
	if agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          row.AgentID,
		WorkspaceID: wsUUID,
	}); err == nil {
		agentName = agent.Name
	}
	pkg, err := teams.BuildAppPackage(teams.ManifestParams{
		AppID:     info.AppID,
		BotName:   info.BotName,
		AgentName: agentName,
		AppURL:    h.TeamsAppURL,
		// Bump on every regeneration so Teams accepts a re-upload as an
		// update: minutes since the install row last changed.
		Version: "1.0." + itoa(row.UpdatedAt.Time.Unix()/60%1_000_000),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := "multica-teams-" + strings.ReplaceAll(strings.ToLower(firstNonEmptyString(info.BotName, "bot")), " ", "-") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(name)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pkg)
}

// RedeemTeamsBindingTokenRequest carries the raw token from the bind link.
type RedeemTeamsBindingTokenRequest struct {
	Token string `json:"token"`
}

// RedeemTeamsBindingTokenResponse echoes the bound identifiers.
type RedeemTeamsBindingTokenResponse struct {
	WorkspaceID    string `json:"workspace_id"`
	InstallationID string `json:"installation_id"`
	TeamsUserID    string `json:"teams_user_id"`
}

// RedeemTeamsBindingToken (POST /api/teams/binding/redeem) binds the Teams
// user carried by the token to the logged-in Multica user.
func (h *Handler) RedeemTeamsBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.TeamsBindingTokens == nil {
		writeFeatureDisabled(w, "teams_not_configured", "teams integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemTeamsBindingTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	redeemed, err := h.TeamsBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, teams.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, teams.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Teams account is already bound to a different Multica user")
		case errors.Is(err, teams.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}
	writeJSON(w, http.StatusOK, RedeemTeamsBindingTokenResponse{
		WorkspaceID:    uuidToString(redeemed.WorkspaceID),
		InstallationID: uuidToString(redeemed.InstallationID),
		TeamsUserID:    redeemed.TeamsUserID,
	})
}

// HandleTeamsWebhook (POST /api/webhooks/teams) is the Bot Framework
// messaging endpoint. Public: authentication is the Connector's JWT, checked
// inside teams.Webhook.
func (h *Handler) HandleTeamsWebhook(w http.ResponseWriter, r *http.Request) {
	if h.TeamsWebhook == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.TeamsWebhook.ServeHTTP(w, r)
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func itoa(n int64) string {
	if n < 0 {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
