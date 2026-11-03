"use client";

// Microsoft Teams settings + per-agent connect flow (fork addition, mirrors
// telegram-tab.tsx). The install model is BYO Azure Bot: the admin pastes the
// bot's App ID, tenant ID and a client secret; the backend verifies them with
// Microsoft before persisting.

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Check, ChevronRight, Copy, Download, Trash2 } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { teamsInstallationsOptions, teamsKeys } from "@multica/core/teams";
import { api } from "@multica/core/api";
import type { TeamsInstallation } from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";
import { useLocale, useT } from "../../i18n";
import { TeamsMark } from "./teams-mark";

function useCanManage(wsId: string): boolean {
  const user = useAuthStore((s) => s.user);
  const { data: members = [] } = useQuery({ ...memberListOptions(wsId), enabled: !!wsId });
  const me = members.find((m) => m.user_id === user?.id) ?? null;
  return me?.role === "owner" || me?.role === "admin";
}

async function downloadAppPackage(wsId: string, inst: TeamsInstallation, failedText: string) {
  try {
    const blob = await api.downloadTeamsAppPackage(wsId, inst.id);
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    const slug = (inst.bot_name || "bot").toLowerCase().replace(/[^a-z0-9]+/g, "-");
    a.download = `multica-teams-${slug}.zip`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (e) {
    toast.error(e instanceof Error ? e.message : failedText);
  }
}

// MessagingEndpoint shows the URL the admin pastes into the Azure Bot.
function MessagingEndpoint({ url }: { url: string }) {
  const { t } = useT("settings");
  const [copied, setCopied] = useState(false);
  if (!url) return null;
  return (
    <div className="space-y-1.5" data-testid="teams-messaging-endpoint">
      <Label>{t(($) => $.teams.endpoint_label)}</Label>
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded-sm bg-muted px-2 py-1.5 text-caption">
          {url}
        </code>
        <Button
          variant="outline"
          size="sm"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(url);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            } catch {
              /* clipboard blocked: the URL is still visible to copy by hand */
            }
          }}
        >
          {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
          {copied ? t(($) => $.teams.copied) : t(($) => $.teams.copy)}
        </Button>
      </div>
      <p className="text-micro text-muted-foreground">{t(($) => $.teams.endpoint_hint)}</p>
    </div>
  );
}

// TeamsTab is the workspace settings panel for Teams bot installations.
export function TeamsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const canManage = useCanManage(wsId);

  const { data, isLoading, isError } = useQuery({
    ...teamsInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installations = data?.installations ?? [];
  const configured = data?.configured === true;

  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteTeamsInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: teamsKeys.installations(wsId) });
      toast.success(t(($) => $.teams.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.teams.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className="space-y-8">
      {isError ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">{t(($) => $.teams.load_failed)}</p>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">{t(($) => $.teams.loading)}</p>
          </CardContent>
        </Card>
      ) : !configured ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-body font-medium">{t(($) => $.teams.not_enabled_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.teams.not_enabled_description_prefix)}{" "}
              <code className="rounded-xs bg-muted px-1 py-0.5 text-micro">
                MULTICA_TEAMS_SECRET_KEY
              </code>{" "}
              {t(($) => $.teams.not_enabled_description_suffix)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <>
          {canManage && data?.messaging_endpoint ? (
            <Card>
              <CardContent>
                <MessagingEndpoint url={data.messaging_endpoint} />
              </CardContent>
            </Card>
          ) : null}
          <section className="space-y-3">
            <h2 className="text-body font-semibold">{t(($) => $.teams.connected_bots)}</h2>
            {installations.length === 0 ? (
              <Card>
                <CardContent className="space-y-2">
                  <p className="text-body font-medium">{t(($) => $.teams.empty_title)}</p>
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.teams.empty_description_prefix)}{" "}
                    <strong>{t(($) => $.teams.empty_description_cta)}</strong>{" "}
                    {t(($) => $.teams.empty_description_suffix)}
                  </p>
                </CardContent>
              </Card>
            ) : (
              <Card>
                <CardContent className="divide-y">
                  {installations.map((inst) => (
                    <InstallationRow
                      key={inst.id}
                      installation={inst}
                      canManage={canManage}
                      onDisconnect={() => setDisconnectTarget(inst.id)}
                    />
                  ))}
                </CardContent>
              </Card>
            )}
          </section>
        </>
      )}

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.teams.disconnect_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.teams.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.teams.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting ? t(($) => $.teams.disconnecting) : t(($) => $.teams.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function InstallationRow({
  installation,
  canManage,
  onDisconnect,
}: {
  installation: TeamsInstallation;
  canManage: boolean;
  onDisconnect: () => void;
}) {
  const { t } = useT("settings");
  const locale = useLocale();
  const wsId = useWorkspaceId();
  const { getAgentName } = useActorName();
  const isActive = installation.status === "active";
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex min-w-0 items-start gap-3">
        <ActorAvatar
          actorType="agent"
          actorId={installation.agent_id}
          size="lg"
          enableHoverCard
          profileLink
        />
        <div className="min-w-0 space-y-1">
          <p className="text-body font-medium">
            {getAgentName(installation.agent_id)}
            {installation.bot_name ? (
              <span className="ml-2 text-caption text-muted-foreground">{installation.bot_name}</span>
            ) : null}
            {!isActive && (
              <span className="ml-2 rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">
                {t(($) => $.teams.revoked_badge)}
              </span>
            )}
          </p>
          <p className="truncate text-micro text-muted-foreground">
            {t(($) => $.teams.app_id_label)}: {installation.app_id}
          </p>
          <p className="text-micro text-muted-foreground">
            {t(($) => $.teams.installed_at_label, {
              when: new Date(installation.installed_at).toLocaleString(locale),
            })}
          </p>
        </div>
      </div>
      {canManage && isActive && (
        <div className="flex shrink-0 flex-wrap justify-end gap-2">
          <TooltipHint label={t(($) => $.teams.download_app_tooltip)}>
            <Button
              variant="outline"
              size="sm"
              onClick={() => downloadAppPackage(wsId, installation, t(($) => $.teams.download_failed_toast))}
              data-testid="teams-download-app"
            >
              <Download className="h-3 w-3" />
              {t(($) => $.teams.download_app)}
            </Button>
          </TooltipHint>
          <Button variant="outline" size="sm" onClick={onDisconnect}>
            <Trash2 className="h-3 w-3" />
            {t(($) => $.teams.disconnect)}
          </Button>
        </div>
      )}
    </div>
  );
}

// TeamsAgentBindButton is the per-agent connect CTA on the agent page.
export function TeamsAgentBindButton({
  agentId,
  agentName,
  className,
  onShowConnectedDetails,
}: {
  agentId: string;
  agentName?: string;
  className?: string;
  onShowConnectedDetails?: () => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const canManage = useCanManage(wsId);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [appId, setAppId] = useState("");
  const [tenantId, setTenantId] = useState("");
  const [secret, setSecret] = useState("");
  const [botName, setBotName] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const { data: listing } = useQuery({ ...teamsInstallationsOptions(wsId), enabled: !!wsId });
  const installSupported = listing?.install_supported === true;

  if (!canManage) return null;

  const existing = listing?.installations.find(
    (inst) => inst.agent_id === agentId && inst.status === "active",
  );
  if (existing) {
    return onShowConnectedDetails ? (
      <button
        type="button"
        onClick={onShowConnectedDetails}
        className={cn(
          "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-caption text-muted-foreground transition-colors hover:bg-muted",
          className,
        )}
        data-testid="teams-agent-bot-status"
      >
        <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
        <span className="truncate">{t(($) => $.teams.agent_bot_connected_label)}</span>
        <ChevronRight className="ml-auto h-3.5 w-3.5 shrink-0" />
      </button>
    ) : (
      <TeamsAgentBotConnectedBadge installation={existing} className={className} />
    );
  }
  if (!installSupported) return null;

  function reset() {
    setAppId("");
    setTenantId("");
    setSecret("");
    setBotName("");
  }
  function closeDialog() {
    if (submitting) return;
    setDialogOpen(false);
    reset();
  }

  const canSubmit = !!appId.trim() && !!tenantId.trim() && !!secret.trim() && !submitting;

  async function handleSubmit() {
    if (!canSubmit || !agentId) return;
    setSubmitting(true);
    try {
      const inst = await api.registerTeamsBot(wsId, agentId, {
        app_id: appId.trim(),
        tenant_id: tenantId.trim(),
        app_secret: secret.trim(),
        bot_name: botName.trim() || agentName || undefined,
      });
      if (!inst.id || inst.status !== "active") {
        throw new Error("Teams connection returned an invalid installation");
      }
      await qc.invalidateQueries({ queryKey: teamsKeys.installations(wsId) });
      toast.success(t(($) => $.teams.connect_success_toast));
      setDialogOpen(false);
      reset();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.teams.connect_failed_toast));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className={cn("flex flex-wrap items-center gap-2", className)} data-testid="teams-agent-bind-buttons">
      <TooltipHint
        label={agentName ? t(($) => $.teams.bind_button_title, { agent: agentName }) : undefined}
      >
        <Button
          variant="outline"
          size="sm"
          onClick={() => setDialogOpen(true)}
          disabled={!agentId}
          data-testid="teams-agent-connect"
        >
          <TeamsMark className="h-3 w-3" />
          {t(($) => $.teams.bind_button)}
        </Button>
      </TooltipHint>

      <Dialog open={dialogOpen} onOpenChange={(v) => (v ? setDialogOpen(true) : closeDialog())}>
        <DialogContent className="sm:max-w-lg" data-testid="teams-connect-dialog">
          <DialogHeader>
            <DialogTitle>{t(($) => $.teams.connect_dialog_title)}</DialogTitle>
          </DialogHeader>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.teams.connect_dialog_description)}
          </p>
          <MessagingEndpoint url={listing?.messaging_endpoint ?? ""} />
          <Field id="teams-app-id" label={t(($) => $.teams.app_id_input_label)} value={appId} onChange={setAppId} disabled={submitting} />
          <Field id="teams-tenant-id" label={t(($) => $.teams.tenant_id_input_label)} value={tenantId} onChange={setTenantId} disabled={submitting} />
          <Field id="teams-secret" label={t(($) => $.teams.secret_input_label)} value={secret} onChange={setSecret} disabled={submitting} secret />
          <Field id="teams-bot-name" label={t(($) => $.teams.bot_name_input_label)} value={botName} onChange={setBotName} disabled={submitting} />
          <DialogFooter>
            <Button variant="outline" size="sm" onClick={closeDialog} disabled={submitting}>
              {t(($) => $.teams.connect_cancel)}
            </Button>
            <Button size="sm" onClick={handleSubmit} disabled={!canSubmit} data-testid="teams-connect-submit">
              {submitting ? t(($) => $.teams.connect_submitting) : t(($) => $.teams.connect_submit)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function Field({
  id,
  label,
  value,
  onChange,
  disabled,
  secret,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  disabled: boolean;
  secret?: boolean;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        data-testid={id}
        type={secret ? "password" : "text"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        disabled={disabled}
      />
    </div>
  );
}

function TeamsAgentBotConnectedBadge({
  installation,
  className,
}: {
  installation: TeamsInstallation;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteTeamsInstallation(wsId, installation.id);
      await qc.invalidateQueries({ queryKey: teamsKeys.installations(wsId) });
      toast.success(t(($) => $.teams.toast_disconnected));
      setConfirmOpen(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.teams.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className={cn("space-y-2", className)} data-testid="teams-agent-bot-connected">
      <div className="flex items-center justify-between gap-3">
        <span className="inline-flex min-w-0 items-center gap-2 text-caption text-muted-foreground">
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
          <span className="truncate">
            {t(($) => $.teams.agent_bot_connected_label)}
            {installation.bot_name ? ` · ${installation.bot_name}` : ""}
          </span>
        </span>
        <div className="flex gap-2">
          <TooltipHint label={t(($) => $.teams.download_app_tooltip)}>
            <Button
              variant="outline"
              size="sm"
              onClick={() => downloadAppPackage(wsId, installation, t(($) => $.teams.download_failed_toast))}
            >
              <Download className="h-3 w-3" />
              {t(($) => $.teams.download_app)}
            </Button>
          </TooltipHint>
          <TooltipHint label={t(($) => $.teams.agent_bot_disconnect_tooltip)}>
            <Button
              variant="destructive"
              size="sm"
              onClick={() => setConfirmOpen(true)}
              disabled={disconnecting}
              aria-label={t(($) => $.teams.disconnect)}
            >
              <Trash2 className="h-3 w-3" />
              {disconnecting ? t(($) => $.teams.disconnecting) : t(($) => $.teams.disconnect)}
            </Button>
          </TooltipHint>
        </div>
      </div>
      <AlertDialog
        open={confirmOpen}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setConfirmOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.teams.disconnect_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.teams.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.teams.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting ? t(($) => $.teams.disconnecting) : t(($) => $.teams.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
