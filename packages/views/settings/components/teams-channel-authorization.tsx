"use client";

// Microsoft Teams connect step for the shared channel dialog (fork addition,
// xsense/multica). The install model is a BYO bot registration: the admin
// pastes the bot's App ID, tenant ID and a client secret; the backend verifies
// them with Microsoft before persisting the secret encrypted.

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { teamsInstallationsOptions, teamsKeys } from "@multica/core/teams";
import type { TeamsInstallation } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { DialogFooter } from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Check, Copy } from "@multica/ui/icons";
import { useT } from "../../i18n";

/** Saves the generated Teams app package (.zip) for upload in Teams admin. */
export async function downloadTeamsAppPackage(
  installation: TeamsInstallation,
  failedText: string,
) {
  try {
    const blob = await api.downloadTeamsAppPackage(
      installation.workspace_id,
      installation.id,
    );
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    const slug = (installation.bot_name || "bot")
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-");
    a.download = `multica-teams-${slug}.zip`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (e) {
    toast.error(e instanceof Error ? e.message : failedText);
  }
}

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
          type="button"
          variant="outline"
          size="sm"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(url);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            } catch {
              // Clipboard blocked: the URL is still visible to copy by hand.
            }
          }}
        >
          {copied ? <Check /> : <Copy />}
          {copied ? t(($) => $.teams.copied) : t(($) => $.teams.copy)}
        </Button>
      </div>
      <p className="text-micro text-muted-foreground">
        {t(($) => $.teams.endpoint_hint)}
      </p>
    </div>
  );
}

export function TeamsChannelAuthorization({
  wsId,
  agentId,
  agentName,
  onClose,
  onBusyChange,
}: {
  wsId: string;
  agentId: string;
  agentName: string;
  onClose: () => void;
  onBusyChange: (busy: boolean) => void;
}) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const { data: listing } = useQuery(teamsInstallationsOptions(wsId));
  const [values, setValues] = useState({
    appId: "",
    tenantId: "",
    secret: "",
    botName: "",
  });
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fields: {
    key: keyof typeof values;
    label: string;
    secret?: boolean;
    optional?: boolean;
  }[] = [
    { key: "appId", label: t(($) => $.teams.app_id_input_label) },
    { key: "tenantId", label: t(($) => $.teams.tenant_id_input_label) },
    {
      key: "secret",
      label: t(($) => $.teams.secret_input_label),
      secret: true,
    },
    {
      key: "botName",
      label: t(($) => $.teams.bot_name_input_label),
      optional: true,
    },
  ];

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    onBusyChange(true);
    setError(null);
    try {
      const installation = await api.registerTeamsBot(wsId, agentId, {
        app_id: values.appId.trim(),
        tenant_id: values.tenantId.trim(),
        app_secret: values.secret.trim(),
        bot_name: values.botName.trim() || agentName || undefined,
      });
      if (!installation.id || installation.status !== "active")
        throw new Error(t(($) => $.teams.connect_failed_toast));
      await qc.invalidateQueries({ queryKey: teamsKeys.installations(wsId) });
      toast.success(t(($) => $.teams.connect_success_toast));
      onClose();
    } catch (reason) {
      setError(
        reason instanceof Error
          ? reason.message
          : t(($) => $.teams.connect_failed_toast),
      );
    } finally {
      setSubmitting(false);
      onBusyChange(false);
    }
  };

  return (
    <form
      onSubmit={submit}
      className="grid gap-4"
      aria-busy={submitting}
      data-testid="teams-connect-form"
    >
      <p className="text-caption text-muted-foreground">
        {t(($) => $.teams.connect_dialog_description)}
      </p>
      <MessagingEndpoint url={listing?.messaging_endpoint ?? ""} />
      {fields.map((field) => (
        <div key={field.key} className="space-y-1.5">
          <Label htmlFor={`teams-${field.key}`}>{field.label}</Label>
          <Input
            id={`teams-${field.key}`}
            type={field.secret ? "password" : "text"}
            autoComplete="off"
            spellCheck={false}
            required={!field.optional}
            disabled={submitting}
            value={values[field.key]}
            onChange={(event) =>
              setValues((old) => ({ ...old, [field.key]: event.target.value }))
            }
          />
        </div>
      ))}
      {error ? (
        <p role="alert" className="text-caption text-destructive">
          {error}
        </p>
      ) : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          disabled={submitting}
          onClick={onClose}
        >
          {t(($) => $.teams.connect_cancel)}
        </Button>
        <Button
          type="submit"
          disabled={
            submitting ||
            fields.some((field) => !field.optional && !values[field.key].trim())
          }
        >
          {submitting
            ? t(($) => $.teams.connect_submitting)
            : t(($) => $.teams.connect_submit)}
        </Button>
      </DialogFooter>
    </form>
  );
}
