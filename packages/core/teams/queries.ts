import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query keys for Teams installations. Realtime sync invalidates
 * `installations(wsId)` on `teams_installation:*` events. */
export const teamsKeys = {
  all: (wsId: string) => ["teams", wsId] as const,
  installations: (wsId: string) => [...teamsKeys.all(wsId), "installations"] as const,
};

export const teamsInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: teamsKeys.installations(wsId),
    queryFn: () => api.listTeamsInstallations(wsId),
    enabled: !!wsId,
  });
