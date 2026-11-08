"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Trash2 } from "@multica/ui/icons";
import { MatrixMark } from "./matrix-mark";
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
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { matrixInstallationsOptions, matrixKeys } from "@multica/core/matrix";
import { api } from "@multica/core/api";
import type { MatrixInstallation } from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";
import { useLocale, useT } from "../../i18n";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";

// MatrixTab is the workspace settings panel for Matrix bot installations,
// mirroring TelegramTab: listing is member-visible; the disconnect action is
// admin-only (backend-enforced; the UI hides the button to match). Adding a
// new installation flows through the Agent detail page — the install path is
// per-agent (one bot per agent, the (workspace_id, agent_id, channel_type)
// UNIQUE in channel_installation).
export function MatrixTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data, isLoading, isError } = useQuery({
    ...matrixInstallationsOptions(wsId),
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
      // Await the server before touching cache/UI (repo rule: no optimistic
      // removal on flows that confirm/destroy).
      await api.deleteMatrixInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: matrixKeys.installations(wsId) });
      toast.success(t(($) => $.matrix.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.matrix.toast_disconnect_failed),
      );
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className="space-y-8">
      {isError ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">
              {t(($) => $.matrix.load_failed)}
            </p>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card>
          <CardContent>
            <p className="text-body text-muted-foreground">{t(($) => $.matrix.loading)}</p>
          </CardContent>
        </Card>
      ) : !configured ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-body font-medium">{t(($) => $.matrix.not_enabled_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.matrix.not_enabled_description_prefix)}{" "}
              <code className="rounded-xs bg-muted px-1 py-0.5 text-micro">
                MULTICA_MATRIX_SECRET_KEY
              </code>{" "}
              {t(($) => $.matrix.not_enabled_description_suffix)}{" "}
              {t(($) => $.matrix.not_enabled_self_host_hint)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <section className="space-y-3">
          <h2 className="text-body font-semibold">{t(($) => $.matrix.connected_bots)}</h2>
          {installations.length === 0 ? (
            <Card>
              <CardContent className="space-y-2">
                <p className="text-body font-medium">{t(($) => $.matrix.empty_title)}</p>
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.matrix.empty_description_prefix)}{" "}
                  <strong>{t(($) => $.matrix.empty_description_cta)}</strong>{" "}
                  {t(($) => $.matrix.empty_description_suffix)}
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
      )}

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.matrix.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.matrix.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.matrix.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.matrix.disconnecting)
                : t(($) => $.matrix.disconnect)}
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
  installation: MatrixInstallation;
  canManage: boolean;
  onDisconnect: () => void;
}) {
  const { t } = useT("settings");
  const locale = useLocale();
  const { getAgentName } = useActorName();
  const isActive = installation.status === "active";
  const agentName = getAgentName(installation.agent_id);
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex items-start gap-3">
        <ActorAvatar
          actorType="agent"
          actorId={installation.agent_id}
          size="lg"
          enableHoverCard
          profileLink
        />
        <div className="space-y-1">
          <p className="text-body font-medium">
            {agentName}
            {installation.user_id ? (
              <span className="ml-2 text-caption text-muted-foreground">
                {installation.user_id}
              </span>
            ) : null}
            {!isActive && (
              <span className="ml-2 rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">
                {t(($) => $.matrix.revoked_badge)}
              </span>
            )}
          </p>
          <p className="text-micro text-muted-foreground">
            {t(($) => $.matrix.installed_at_label, {
              when: new Date(installation.installed_at).toLocaleString(locale),
            })}
          </p>
        </div>
      </div>
      {canManage && isActive && (
        <Button variant="outline" size="sm" onClick={onDisconnect}>
          <Trash2 className="h-3 w-3" />
          {t(($) => $.matrix.disconnect)}
        </Button>
      )}
    </div>
  );
}

// MatrixAgentBindButton is the per-agent CTA on the agent detail page.
// Matrix uses the paste-a-credential model: the admin pastes the homeserver
// URL and the bot's access token; the backend validates via whoami before
// persisting. Visibility mirrors TelegramAgentBindButton (owner/admin only).
export function MatrixAgentBindButton({
  agentId,
  agentName,
  className,
}: {
  agentId: string;
  agentName?: string;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [homeserver, setHomeserver] = useState("");
  const [accessToken, setAccessToken] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const { data: listing } = useQuery({
    ...matrixInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installSupported = listing?.install_supported === true;

  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  if (!canManage) return null;

  const existing = listing?.installations.find(
    (inst) => inst.agent_id === agentId && inst.status === "active",
  );
  if (existing) {
    return (
      <div
        className={cn("inline-flex items-center gap-2", className)}
        data-testid="matrix-agent-bot-connected"
      >
        <span className="inline-flex items-center gap-2 text-caption text-muted-foreground">
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500" />
          <span className="truncate">{t(($) => $.matrix.agent_bot_connected_label)}</span>
        </span>
      </div>
    );
  }

  if (!installSupported) return null;

  function closeDialog() {
    if (submitting) return;
    setDialogOpen(false);
    setHomeserver("");
    setAccessToken("");
  }

  async function handleSubmit() {
    const homeserverTrimmed = homeserver.trim();
    const access_token = accessToken.trim();
    if (submitting || !agentId || !homeserverTrimmed || !access_token) return;
    setSubmitting(true);
    try {
      const installation = await api.matrixInstallBot(wsId, agentId, {
        homeserver: homeserverTrimmed,
        access_token,
      });
      if (!installation.id || installation.status !== "active") {
        throw new Error("Matrix connection returned an invalid installation");
      }
      // The matrix_installation realtime event also refreshes this list, but
      // invalidate explicitly so the connected badge appears immediately.
      await qc.invalidateQueries({ queryKey: matrixKeys.installations(wsId) });
      toast.success(t(($) => $.matrix.connect_success_toast));
      setDialogOpen(false);
      setHomeserver("");
      setAccessToken("");
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.matrix.connect_failed_toast),
      );
    } finally {
      setSubmitting(false);
    }
  }

  const canSubmit =
    homeserver.trim() !== "" && accessToken.trim() !== "" && !submitting;

  return (
    <div
      className={cn("flex flex-wrap items-center gap-2", className)}
      data-testid="matrix-agent-bind-buttons"
    >
      <TooltipHint
        label={
          agentName
            ? t(($) => $.matrix.bind_button_title, { agent: agentName })
            : undefined
        }
      >
        <Button
          variant="outline"
          size="sm"
          onClick={() => setDialogOpen(true)}
          disabled={!agentId}
          data-testid="matrix-agent-connect"
        >
          <MatrixMark className="h-3 w-3" />
          {t(($) => $.matrix.bind_button)}
        </Button>
      </TooltipHint>

      <Dialog
        open={dialogOpen}
        onOpenChange={(v) => (v ? setDialogOpen(true) : closeDialog())}
      >
        <DialogContent className="sm:max-w-lg" data-testid="matrix-connect-dialog">
          <DialogHeader>
            <DialogTitle>{t(($) => $.matrix.connect_dialog_title)}</DialogTitle>
          </DialogHeader>

          <p className="text-caption text-muted-foreground">
            {t(($) => $.matrix.connect_dialog_description)}
          </p>

          <div className="space-y-1.5">
            <Label htmlFor="matrix-homeserver">
              {t(($) => $.matrix.homeserver_label)}
            </Label>
            <Input
              id="matrix-homeserver"
              data-testid="matrix-homeserver"
              type="url"
              value={homeserver}
              onChange={(e) => setHomeserver(e.target.value)}
              placeholder="https://matrix.example.org"
              autoComplete="off"
              spellCheck={false}
              disabled={submitting}
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="matrix-access-token">
              {t(($) => $.matrix.access_token_label)}
            </Label>
            <Input
              id="matrix-access-token"
              data-testid="matrix-access-token"
              type="password"
              value={accessToken}
              onChange={(e) => setAccessToken(e.target.value)}
              placeholder="syt_…"
              autoComplete="off"
              spellCheck={false}
              disabled={submitting}
            />
          </div>

          <DialogFooter>
            <Button
              variant="outline"
              size="sm"
              onClick={closeDialog}
              disabled={submitting}
            >
              {t(($) => $.matrix.connect_cancel)}
            </Button>
            <Button
              size="sm"
              onClick={handleSubmit}
              disabled={!canSubmit}
              data-testid="matrix-connect-submit"
            >
              {submitting
                ? t(($) => $.matrix.connect_submitting)
                : t(($) => $.matrix.connect_submit)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}