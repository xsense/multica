/** A Microsoft Teams bot (Azure Bot) installation bound to a single Multica
 * agent. Fork addition (xsense/multica).
 *
 * Wire shape mirrors `TeamsInstallationResponse` in
 * `server/internal/handler/teams.go`. */
export interface TeamsInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  /** Microsoft App ID (Entra application id) of the Azure Bot. */
  app_id: string;
  /** Entra tenant the bot is registered in (single-tenant). */
  tenant_id: string;
  /** Display name used for the generated Teams app. */
  bot_name: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListTeamsInstallationsResponse {
  installations: TeamsInstallation[];
  /** Whether MULTICA_TEAMS_SECRET_KEY is set on the server. */
  configured: boolean;
  install_supported?: boolean;
  /** Public URL to paste as the Azure Bot's messaging endpoint. */
  messaging_endpoint?: string;
}

/** Request body for a bot install: the Azure Bot's credentials. */
export interface RegisterTeamsRequest {
  app_id: string;
  tenant_id: string;
  app_secret: string;
  bot_name?: string;
}

export interface RedeemTeamsBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  teams_user_id: string;
}
