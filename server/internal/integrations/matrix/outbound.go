package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file is the Phase 1 Matrix outbound: it listens on the process bus for
// the final agent reply (EventChatDone) and sends it to the room as a single
// m.room.message. Unlike the Telegram outbound it does NOT stream partial
// frames or edit a placeholder — Phase 1 delivers the answer once, in full.
// Streaming edits and per-frame throttling are a follow-up.

// outboundQueries is the slice of generated queries the subscriber needs.
// *db.Queries satisfies it.
type outboundQueries interface {
	GetChannelTaskDelivery(ctx context.Context, taskID pgtype.UUID) (db.ChannelTaskDelivery, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// Outbound delivers final chat-done replies to Matrix rooms.
type Outbound struct {
	q       outboundQueries
	decrypt Decrypter
	logger  *slog.Logger
	client  *http.Client
}

// NewOutbound builds the outbound subscriber. client optional (tests); nil
// uses the default per-call client.
func NewOutbound(q outboundQueries, decrypt Decrypter, logger *slog.Logger, client *http.Client) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	return &Outbound{q: q, decrypt: decrypt, logger: logger, client: client}
}

// Register subscribes to the transcript completion + failure events.
func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, o.handleChatDone)
	bus.Subscribe(protocol.EventTaskFailed, o.handleTaskFailed)
	bus.Subscribe(protocol.EventTaskCancelled, o.handleTaskCancelled)
}

// resolveTarget maps the event's task delivery to Matrix credentials + room.
// A nil, nil return means the task has no Matrix route (web/desktop chat, or a
// different channel) and must not reach an external conversation.
func (o *Outbound) resolveTarget(ctx context.Context, taskID pgtype.UUID) (*outboundTarget, error) {
	if !taskID.Valid {
		return nil, nil
	}
	delivery, err := o.q.GetChannelTaskDelivery(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup matrix task delivery: %w", err)
	}
	if delivery.ChannelType != string(TypeMatrix) {
		return nil, nil
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          delivery.InstallationID,
		ChannelType: string(TypeMatrix),
	})
	if err != nil {
		return nil, fmt.Errorf("load matrix installation: %w", err)
	}
	if inst.Status != "active" {
		return nil, nil // revoked between trigger and reply
	}
	creds, err := decodeCredentials(inst.Config, o.decrypt)
	if err != nil {
		return nil, fmt.Errorf("decode matrix credentials: %w", err)
	}
	roomID := outboundRoom(delivery)
	if roomID == "" {
		return nil, nil
	}
	return &outboundTarget{roomID: roomID, creds: creds, installationID: inst.ID}, nil
}

// outboundRoom recovers the room id from the task-delivery row. The room id
// lives on ChannelChatID for the general case and is also persisted in the
// binding config for stability across binding-key changes, mirroring
// telegram.outboundTarget.
func outboundRoom(delivery db.ChannelTaskDelivery) string {
	var bc matrixBindingConfig
	if len(delivery.Config) > 0 && json.Unmarshal(delivery.Config, &bc) == nil && bc.RoomID != "" {
		return bc.RoomID
	}
	return delivery.ChannelChatID
}

// outboundTarget bundles the resolved send destination.
type outboundTarget struct {
	roomID         string
	creds          credentials
	installationID pgtype.UUID
}

// handleChatDone sends the final agent answer to the room. Empty or
// non-text content is ignored.
func (o *Outbound) handleChatDone(e events.Event) {
	content, ok := chatDoneContent(e.Payload)
	if !ok {
		o.logger.DebugContext(context.Background(), "matrix outbound: chat-done has no text content", "task_id", e.TaskID, "payload", fmt.Sprintf("%T", e.Payload))
		return
	}
	taskID, _ := eventTaskID(e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := o.resolveTarget(ctx, taskID)
	if err != nil || t == nil {
		if err != nil {
			o.logger.WarnContext(ctx, "matrix outbound: resolve chat-done target failed", "task_id", taskID.String(), "error", err)
		} else {
			o.logger.DebugContext(ctx, "matrix outbound: chat-done has no matrix route (nil target)", "task_id", taskID.String())
		}
		return
	}
	api := newMatrixAPI(t.creds.Homeserver, t.creds.AccessToken, o.client)
	eventID, err := api.sendMessage(ctx, t.roomID, txnID(), map[string]any{
		"msgtype": "m.text",
		"body":    content,
	})
	if err != nil {
		o.logger.WarnContext(ctx, "matrix outbound: send chat-done reply failed",
			"installation_id", util.UUIDToString(t.installationID), "room_id", t.roomID, "error", err)
		return
	}
	o.logger.InfoContext(ctx, "matrix outbound: sent chat-done reply",
		"installation_id", util.UUIDToString(t.installationID), "room_id", t.roomID, "event_id", eventID)
}

func (o *Outbound) handleTaskFailed(e events.Event) {
	o.sendFailureNotice(e, "Your agent task failed. You can ask it to retry.")
}

func (o *Outbound) handleTaskCancelled(e events.Event) {
	o.sendFailureNotice(e, "Your agent task was cancelled before it finished.")
}

// sendFailureNotice posts a short failure notice back to the room, best-effort.
func (o *Outbound) sendFailureNotice(e events.Event, text string) {
	taskID, _ := eventTaskID(e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := o.resolveTarget(ctx, taskID)
	if err != nil || t == nil {
		return
	}
	api := newMatrixAPI(t.creds.Homeserver, t.creds.AccessToken, o.client)
	if _, err := api.sendMessage(ctx, t.roomID, txnID(), map[string]any{
		"msgtype": "m.text",
		"body":    text,
	}); err != nil {
		o.logger.WarnContext(ctx, "matrix outbound: failure notice failed",
			"installation_id", util.UUIDToString(t.installationID), "error", err)
	}
}

// chatDoneContent extracts the reply text from an EventChatDone payload.
func chatDoneContent(payload any) (string, bool) {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		if p.Content == "" {
			return "", false
		}
		return p.Content, true
	case map[string]any:
		s, _ := p["content"].(string)
		if s == "" {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}

// eventTaskID extracts the task id from the event envelope.
func eventTaskID(e events.Event) (pgtype.UUID, bool) {
	raw := e.TaskID
	if raw == "" {
		switch p := e.Payload.(type) {
		case protocol.ChatDonePayload:
			raw = p.TaskID
		case map[string]any:
			raw, _ = p["task_id"].(string)
		}
	}
	id, err := util.ParseUUID(raw)
	return id, err == nil && id.Valid
}

// txnID returns a unique client transaction id for send idempotency.
func txnID() string {
	return dbid.NewV7().String()
}