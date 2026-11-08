package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// matrix_handler_test.go mirrors telegram_test.go: the not-configured gate must
// return empty (list) and 403 (mutations), and the management response must
// never expose the stored access token.

func TestListMatrixInstallationsNotConfiguredReturnsEmpty(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/x/matrix/installations", nil)
	w := httptest.NewRecorder()

	h.ListMatrixInstallations(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Installations    []any `json:"installations"`
		Configured       bool  `json:"configured"`
		InstallSupported bool  `json:"install_supported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Configured || resp.InstallSupported || len(resp.Installations) != 0 {
		t.Fatalf("unexpected unconfigured response: %+v", resp)
	}
}

func TestMatrixMutationHandlersRejectUnconfiguredDeployment(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		run    func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "register",
			method: http.MethodPost,
			path:   "/api/workspaces/x/matrix/install?agent_id=y",
			body:   `{"homeserver":"https://hs","access_token":"placeholder"}`,
			run:    (*Handler).RegisterMatrixBot,
		},
		{
			name:   "revoke",
			method: http.MethodDelete,
			path:   "/api/workspaces/x/matrix/installations/y",
			run:    (*Handler).RevokeMatrixInstallation,
		},
		{
			name:   "bind user",
			method: http.MethodPost,
			path:   "/api/workspaces/x/matrix/installations/y/bindings",
			body:   `{"matrix_user_id":"@a:hs","multica_user_id":"z"}`,
			run:    (*Handler).BindMatrixUser,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			w := httptest.NewRecorder()

			tt.run(h, w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMatrixInstallationResponseNeverExposesStoredCredential(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	row := db.ChannelInstallation{
		ID:              parseUUID("11111111-1111-1111-1111-111111111111"),
		WorkspaceID:     parseUUID("22222222-2222-2222-2222-222222222222"),
		AgentID:         parseUUID("33333333-3333-3333-3333-333333333333"),
		InstallerUserID: parseUUID("44444444-4444-4444-4444-444444444444"),
		Status:          "active",
		Config: json.RawMessage(
			`{"app_id":"@my_test_bot:example.org","homeserver":"https://matrix.example.org","access_token_encrypted":"ciphertext-sentinel"}`,
		),
		InstalledAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}

	got := matrixInstallationToResponse(row)
	if got.UserID != "@my_test_bot:example.org" || got.Homeserver != "https://matrix.example.org" {
		t.Fatalf("public matrix identity = %+v", got)
	}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(payload), "ciphertext-sentinel") ||
		strings.Contains(string(payload), "access_token") ||
		strings.Contains(string(payload), "token_encrypted") {
		t.Fatalf("management response exposed stored credential: %s", payload)
	}
}