package matrix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// install_test.go covers the install flow risks with an in-memory fake of the
// queries interface plus an httptest homeserver that serves whoami: a valid
// token persists a sealed config keyed by the bot user id, a rejected token
// yields ErrCredentialsRejected, and a malformed homeserver is refused without
// a network call.

type fakeInstallQueries struct {
	upsert     db.UpsertChannelInstallationParams
	appIDTaken bool
	owner      db.GetChannelInstallationOwnerByAppIDRow
	ownerErr   error
	rowID      pgtype.UUID
}

func (f *fakeInstallQueries) WithTx(pgx.Tx) installQueries { return f }
func (f *fakeInstallQueries) ReclaimDeadChannelInstallationByAppID(context.Context, db.ReclaimDeadChannelInstallationByAppIDParams) (pgtype.UUID, error) {
	return pgtype.UUID{}, pgx.ErrNoRows
}
func (f *fakeInstallQueries) UpsertChannelInstallation(_ context.Context, p db.UpsertChannelInstallationParams) (db.ChannelInstallation, error) {
	f.upsert = p
	if f.appIDTaken {
		return db.ChannelInstallation{}, &pgconn.PgError{Code: pgUniqueViolation}
	}
	return db.ChannelInstallation{ID: f.rowID, WorkspaceID: p.WorkspaceID, AgentID: p.AgentID, ChannelType: p.ChannelType, Config: p.Config, InstallerUserID: p.InstallerUserID, Status: "active"}, nil
}
func (f *fakeInstallQueries) GetChannelInstallationOwnerByAppID(context.Context, db.GetChannelInstallationOwnerByAppIDParams) (db.GetChannelInstallationOwnerByAppIDRow, error) {
	return f.owner, f.ownerErr
}
func (f *fakeInstallQueries) ListChannelInstallationsByWorkspace(context.Context, db.ListChannelInstallationsByWorkspaceParams) ([]db.ChannelInstallation, error) {
	return nil, nil
}
func (f *fakeInstallQueries) GetChannelInstallationInWorkspace(context.Context, db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error) {
	return db.ChannelInstallation{}, pgx.ErrNoRows
}
func (f *fakeInstallQueries) SetChannelInstallationStatus(context.Context, db.SetChannelInstallationStatusParams) error {
	return nil
}

type fakeTxStarter struct{}

func (fakeTxStarter) Begin(ctx context.Context) (pgx.Tx, error) { return fakeTx{}, nil }

type fakeTx struct{ pgx.Tx }

func (fakeTx) Commit(context.Context) error   { return nil }
func (fakeTx) Rollback(context.Context) error { return nil }

// whoamiServer returns an httptest server whose whoami is `userID` when healthy
// and 403 M_UNKNOWN_TOKEN when rejected.
func whoamiServer(t *testing.T, userID string, reject bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/account/whoami" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if reject {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": "M_UNKNOWN_TOKEN", "error": "Invalid token"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": userID})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestInstallService(t *testing.T, q installQueries) (*InstallService, *secretbox.Box) {
	t.Helper()
	box := newTestBox(t)
	svc, err := newInstallService(q, fakeTxStarter{}, box, nil)
	if err != nil {
		t.Fatalf("newInstallService: %v", err)
	}
	return svc, box
}

func TestRegisterValidatesCredentialsAndPersistsSealedConfig(t *testing.T) {
	srv := whoamiServer(t, "@bot:example.org", false)
	f := &fakeInstallQueries{rowID: pgtype.UUID{Valid: true, Bytes: [16]byte{1}}}
	svc, _ := newTestInstallService(t, f)

	row, err := svc.Register(context.Background(), RegisterParams{
		WorkspaceID: pgtype.UUID{Valid: true, Bytes: [16]byte{2}},
		AgentID:     pgtype.UUID{Valid: true, Bytes: [16]byte{3}},
		InitiatorID: pgtype.UUID{Valid: true, Bytes: [16]byte{4}},
		Homeserver:  srv.URL,
		AccessToken: "syt_secret",
	})
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if row.ChannelType != string(TypeMatrix) {
		t.Errorf("ChannelType = %q, want matrix", row.ChannelType)
	}
	var ic installConfig
	if err := json.Unmarshal(f.upsert.Config, &ic); err != nil {
		t.Fatalf("stored config is not our installConfig: %v", err)
	}
	if ic.AppID != "@bot:example.org" {
		t.Errorf("stored app_id = %q, want the validated user id", ic.AppID)
	}
	if ic.Homeserver != strings.TrimRight(srv.URL, "/") {
		t.Errorf("stored homeserver = %q, want %q", ic.Homeserver, srv.URL)
	}
	// The token must be encrypted at rest, not plaintext.
	if ic.AccessTokenEncrypted == "" || strings.Contains(ic.AccessTokenEncrypted, "syt_secret") {
		t.Fatalf("access_token_encrypted must not hold plaintext: %q", ic.AccessTokenEncrypted)
	}
	enc, err := base64.StdEncoding.DecodeString(ic.AccessTokenEncrypted)
	if err != nil {
		t.Fatalf("bad base64 access_token_encrypted: %v", err)
	}
	if _, err := newTestBox(t).Open(enc); err != nil {
		t.Fatalf("stored token does not decrypt with the install box: %v", err)
	}
	// The routing slot was seeded with the validated user id so inbound can
	// resolve the installation by the bot's user id.
	if f.upsert.Config == nil {
		t.Fatal("expected a config written to the channel_installation row")
	}
}

func newTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func TestRegisterRejectedTokenIsCredentialsRejected(t *testing.T) {
	srv := whoamiServer(t, "", true)
	f := &fakeInstallQueries{}
	svc, _ := newTestInstallService(t, f)

	if _, err := svc.Register(context.Background(), RegisterParams{
		WorkspaceID: pgtype.UUID{Valid: true, Bytes: [16]byte{2}},
		AgentID:     pgtype.UUID{Valid: true, Bytes: [16]byte{3}},
		InitiatorID: pgtype.UUID{Valid: true, Bytes: [16]byte{4}},
		Homeserver:  srv.URL,
		AccessToken: "syt_bad",
	}); !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("Register error = %v, want ErrCredentialsRejected", err)
	}
	if f.upsert.Config != nil {
		t.Fatal("a rejected token must never be persisted")
	}
}

func TestRegisterMalformedHomeserverRefusedLocally(t *testing.T) {
	f := &fakeInstallQueries{}
	svc, _ := newTestInstallService(t, f)
	// No server needed: a non-http URL is refused before any network call.
	if _, err := svc.Register(context.Background(), RegisterParams{
		WorkspaceID: pgtype.UUID{Valid: true, Bytes: [16]byte{2}},
		AgentID:     pgtype.UUID{Valid: true, Bytes: [16]byte{3}},
		InitiatorID: pgtype.UUID{Valid: true, Bytes: [16]byte{4}},
		Homeserver:  "matrix.example.org", // missing scheme
		AccessToken: "syt_x",
	}); !errors.Is(err, ErrInvalidHomeserver) {
		t.Fatalf("Register error = %v, want ErrInvalidHomeserver", err)
	}
}

func TestInstallServiceConflictClassification(t *testing.T) {
	f := &fakeInstallQueries{appIDTaken: true, rowID: pgtype.UUID{Valid: true, Bytes: [16]byte{1}}}
	svc, _ := newTestInstallService(t, f)
	err := svc.liveOwnerConflictErr(context.Background(), pgtype.UUID{Valid: true, Bytes: [16]byte{2}}, "@bot:example.org")
	if !errors.Is(err, ErrBotOwnedByAnotherWorkspace) {
		t.Fatalf("conflict err = %v, want ErrBotOwnedByAnotherWorkspace when owner lookup fails", err)
	}
}