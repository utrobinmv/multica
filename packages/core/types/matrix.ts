/** A Matrix bot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `MatrixInstallationResponse` in
 * `server/internal/handler/matrix.go`. New fields the backend adds in the
 * future MUST default to optional so older desktop builds keep parsing the
 * response — see CLAUDE.md → API Compatibility. */
export interface MatrixInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  /** The bot's full Matrix user id, e.g. `@ml_dl_research_2:192.168.45.30`. */
  user_id: string;
  /** The homeserver the bot belongs to, e.g. `http://192.168.45.30:25387`. */
  homeserver: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListMatrixInstallationsResponse {
  installations: MatrixInstallation[];
  /** Whether MULTICA_MATRIX_SECRET_KEY is configured on the deployment.
   * When false the install entry points are hidden and the panel renders the
   * "ask the operator to enable Matrix" state. */
  configured: boolean;
  /** Whether the install path is available (true whenever Matrix is
   * configured — an admin-pasted access token needs no hosted credential).
   * Optional so an older build that predates it treats it as off. */
  install_supported?: boolean;
}

/** Request body for a bot install: the homeserver the bot lives on and its
 * access token. The backend validates live (whoami) before persisting. */
export interface RegisterMatrixRequest {
  homeserver: string;
  access_token: string;
}