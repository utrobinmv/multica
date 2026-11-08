package matrix

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Phase 1 Matrix user binding. There is no in-product redeem
// page yet (no frontend), so an admin maps a Matrix user id to a Multica user
// id directly through a management endpoint (handler.BindMatrixUser). The row
// feeds the identity resolver the same way Telegram's self-service binding
// does — a sender's Matrix user id resolves to a Multica user only once a
// binding row exists. This is the minimal surface that makes the chat actually
// ingest a message; a full token-mint/redeem flow is the follow-up.

var (
	// ErrBindingAlreadyAssigned: this Matrix user id is already bound to a
	// (possibly different) Multica user in the installation.
	ErrBindingAlreadyAssigned = errors.New("matrix: user id is already bound to a different user")
	// ErrBindingNotWorkspaceMember: the Multica target user is not a member of
	// the installation's workspace.
	ErrBindingNotWorkspaceMember = errors.New("matrix: target user is not a workspace member")
)

// BindingService creates and lists channel_user_binding rows for a Matrix
// installation.
type BindingService struct {
	q *db.Queries
}

// NewBindingService constructs the service.
func NewBindingService(q *db.Queries) *BindingService {
	return &BindingService{q: q}
}

// BindUser maps matrixUserID -> multicaUserID for the installation. It
// re-checks workspace membership up front (there is no member FK, same rule as
// Telegram's redeem) and refuses to silently reassign a bound user.
func (s *BindingService) BindUser(ctx context.Context, inst db.ChannelInstallation, multicaUserID pgtype.UUID, matrixUserID string) (db.ChannelUserBinding, error) {
	if !multicaUserID.Valid {
		return db.ChannelUserBinding{}, errors.New("matrix: target user id is invalid")
	}
	if _, err := parseUserID(matrixUserID); err != nil {
		return db.ChannelUserBinding{}, err
	}
	// Explicit membership gate (no member FK) — mirrors telegram/binding.go.
	if _, err := s.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      multicaUserID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChannelUserBinding{}, ErrBindingNotWorkspaceMember
		}
		return db.ChannelUserBinding{}, fmt.Errorf("matrix: check membership: %w", err)
	}

	row, err := s.q.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
		WorkspaceID:    inst.WorkspaceID,
		MulticaUserID:  multicaUserID,
		InstallationID: inst.ID,
		ChannelType:    string(TypeMatrix),
		ChannelUserID:  matrixUserID,
		Config:         []byte(`{}`),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChannelUserBinding{}, ErrBindingAlreadyAssigned
		}
		return db.ChannelUserBinding{}, fmt.Errorf("matrix: create binding: %w", err)
	}
	return row, nil
}