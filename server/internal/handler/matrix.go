package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/integrations/matrix"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// MatrixInstallationResponse is the wire shape for a Matrix installation row.
// The encrypted access token in config is INTENTIONALLY absent — it is
// server-internal. WS lease columns are runtime state, not API surface.
type MatrixInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	UserID          string `json:"user_id"`
	Homeserver      string `json:"homeserver"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func matrixInstallationToResponse(row db.ChannelInstallation) MatrixInstallationResponse {
	info := matrix.DecodePublicConfig(row.Config)
	return MatrixInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		UserID:          info.UserID,
		Homeserver:      info.Homeserver,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ListMatrixInstallations (GET /api/workspaces/{id}/matrix/installations)
// is member-visible so the Integrations tab renders for non-admins.
func (h *Handler) ListMatrixInstallations(w http.ResponseWriter, r *http.Request) {
	if h.MatrixInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations":     []MatrixInstallationResponse{},
			"configured":        false,
			"install_supported": false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.MatrixInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list matrix installations")
		return
	}
	out := make([]MatrixInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, matrixInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":     out,
		"configured":        true,
		"install_supported": true,
	})
}

// RegisterMatrixRequest is the body for a bot install: the homeserver base
// URL and the bot's access token.
type RegisterMatrixRequest struct {
	Homeserver  string `json:"homeserver"`
	AccessToken string `json:"access_token"`
}

// RegisterMatrixBot (POST /api/workspaces/{id}/matrix/install?agent_id=…)
// installs a user-supplied Matrix bot for an agent. Admin-only at the router.
func (h *Handler) RegisterMatrixBot(w http.ResponseWriter, r *http.Request) {
	if h.MatrixInstall == nil {
		writeFeatureDisabled(w, "matrix_not_configured", "matrix integration not enabled")
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
	// Ownership pre-check at the boundary so a wrong agent_id is a clear 404.
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterMatrixRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	row, err := h.MatrixInstall.Register(r.Context(), matrix.RegisterParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: initiatorUUID,
		Homeserver:  body.Homeserver,
		AccessToken: body.AccessToken,
	})
	if err != nil {
		switch {
		case errors.Is(err, matrix.ErrInvalidHomeserver):
			writeError(w, http.StatusBadRequest, "the homeserver must be an http(s) URL — check the value and try again")
		case errors.Is(err, matrix.ErrCredentialsRejected):
			writeError(w, http.StatusBadRequest, "the homeserver rejected this access token — generate a fresh token and try again")
		case errors.Is(err, matrix.ErrCredentialsUnverifiable):
			writeError(w, http.StatusServiceUnavailable, "could not reach the homeserver to verify this token — check the server network and try again; the token was not saved")
		case errors.Is(err, matrix.ErrBotOwnedBySameWorkspace):
			writeError(w, http.StatusConflict, "this Matrix bot is already connected to another agent in this workspace — disconnect it there first, then connect it here")
		case errors.Is(err, matrix.ErrBotOwnedByArchivedAgent):
			writeError(w, http.StatusConflict, "this Matrix bot is connected to an archived agent in this workspace — restore that agent, or disconnect its bot, before connecting it here")
		case errors.Is(err, matrix.ErrBotOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this Matrix bot is already connected to a different Multica workspace — disconnect it there before connecting it here")
		default:
			writeError(w, http.StatusInternalServerError, "could not save this Matrix bot — something went wrong on the server; the token was not saved")
		}
		return
	}
	// Broadcast so every open client invalidates its installations query.
	h.publish(protocol.EventMatrixInstallationCreated, uuidToString(row.WorkspaceID), "user", userID, map[string]any{
		"id": uuidToString(row.ID),
	})
	writeJSON(w, http.StatusOK, matrixInstallationToResponse(row))
}

// RevokeMatrixInstallation (DELETE /api/workspaces/{id}/matrix/installations/{installationId})
// flips status to 'revoked'. Admin-only at the router. The row is preserved
// for audit; a re-install (re-pasting the bot's token) flips status back.
func (h *Handler) RevokeMatrixInstallation(w http.ResponseWriter, r *http.Request) {
	if h.MatrixInstall == nil {
		writeFeatureDisabled(w, "matrix_not_configured", "matrix integration not configured")
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
	if _, err := h.MatrixInstall.GetInWorkspace(r.Context(), instUUID, wsUUID); err != nil {
		if errors.Is(err, matrix.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "matrix installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.MatrixInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventMatrixInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// BindMatrixUserRequest carries the Matrix user id to bind and the Multica
// user id of the target workspace member.
type BindMatrixUserRequest struct {
	MatrixUserID string `json:"matrix_user_id"`
	MulticaUserID string `json:"multica_user_id"`
}

// BindMatrixUser (POST /api/workspaces/{id}/matrix/installations/{installationId}/bindings)
// maps a Matrix user id to a Multica user so the sender can chat with the bot.
// Admin-only at the router. Phase 1 admin surface; a self-service redeem flow
// is the follow-up.
func (h *Handler) BindMatrixUser(w http.ResponseWriter, r *http.Request) {
	if h.MatrixInstall == nil || h.MatrixBinding == nil {
		writeFeatureDisabled(w, "matrix_not_configured", "matrix integration not configured")
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
	inst, err := h.MatrixInstall.GetInWorkspace(r.Context(), instUUID, wsUUID)
	if err != nil {
		if errors.Is(err, matrix.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "matrix installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	var body BindMatrixUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	targetUUID, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(body.MulticaUserID), "multica_user_id")
	if !ok {
		return
	}
	binding, err := h.MatrixBinding.BindUser(r.Context(), inst, targetUUID, strings.TrimSpace(body.MatrixUserID))
	if err != nil {
		switch {
		case errors.Is(err, matrix.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "the target user is not a member of this workspace")
		case errors.Is(err, matrix.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Matrix user is already bound to a different user in this installation")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	_ = userID
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":              uuidToString(binding.ID),
		"matrix_user_id":  binding.ChannelUserID,
		"multica_user_id": uuidToString(binding.MulticaUserID),
	})
}