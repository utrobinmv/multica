import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for everything Matrix-installation-related. Realtime
 * sync invalidates `installations(wsId)` on `matrix_installation:*` events
 * so the Settings panel updates without a manual refetch. */
export const matrixKeys = {
  all: (wsId: string) => ["matrix", wsId] as const,
  installations: (wsId: string) => [...matrixKeys.all(wsId), "installations"] as const,
};

export const matrixInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: matrixKeys.installations(wsId),
    queryFn: () => api.listMatrixInstallations(wsId),
    enabled: !!wsId,
  });