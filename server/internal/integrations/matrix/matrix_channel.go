package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// matrixChannel is ONE installation's /sync long-polling loop. Matrix offers
// no WebSocket push transport over plain CS API; long polling is the
// persistent-connection equivalent — one blocking loop per bot account,
// supervised per-installation by engine.Supervisor exactly like Telegram's
// getUpdates loop.
type matrixChannel struct {
	botID        string // bot Matrix user id, e.g. @bot:example.org
	api          *matrixAPI
	handler      channel.InboundHandler
	logger       *slog.Logger
	acceptsMedia bool
}

// pollRetryDelay spaces retries after a transient /sync failure inside one
// Connect attempt before giving the error to the Supervisor's backoff.
const pollRetryDelay = 2 * time.Second

func (c *matrixChannel) Type() channel.Type { return TypeMatrix }

func (c *matrixChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapThreadReply | channel.CapAttachment
}

// Disconnect is a no-op: the polling loop's whole lifetime is scoped to
// Connect (it returns when the run context is cancelled).
func (c *matrixChannel) Disconnect(ctx context.Context) error { return nil }

// Send posts an outbound reply with this installation's bot token.
func (c *matrixChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	txnID := fmt.Sprintf("multica-%d", time.Now().UnixNano())
	eventID, err := c.api.sendMessage(ctx, out.ChatID, txnID, map[string]any{
		"msgtype": "m.text",
		"body":    out.Text,
	})
	if err != nil {
		return channel.SendResult{}, err
	}
	return channel.SendResult{MessageID: eventID}, nil
}

// Connect runs the /sync long-polling receive loop until ctx is cancelled
// (returns nil — graceful stop) or the link fails in a way the loop cannot
// absorb (returns the error — the Supervisor reconnects under backoff). A
// rejected token is surfaced as ErrConflict with a precise message: it means
// the access token was rotated/revoked, a state backoff cannot fix but the
// operator can.
func (c *matrixChannel) Connect(ctx context.Context) error {
	if c.handler == nil {
		return errors.New("matrix: inbound handler not configured")
	}
	var since string
	for {
		res, err := c.api.sync(ctx, since, syncTimeoutSecs*1000)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrConflict) {
				c.logger.WarnContext(ctx, "matrix: /sync access token rejected — rotate or re-install the bot; this token is no longer accepted",
					"bot_id", c.botID)
				return err
			}
			// Transient network/API failure: one spaced retry loop inside the
			// attempt keeps a momentary blip from churning the Supervisor's
			// backoff; persistent failure still escalates via repeated errors.
			c.logger.WarnContext(ctx, "matrix: /sync failed", "bot_id", c.botID, "error", err)
			if !sleepCtx(ctx, pollRetryDelay) {
				return nil
			}
			return fmt.Errorf("matrix: /sync: %w", err)
		}
		for roomID, room := range res.Rooms.Join {
			for _, rawEvent := range room.Timeline.Events {
				if err := c.dispatch(ctx, roomID, rawEvent); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
			}
		}
		if res.NextBatch != "" {
			since = res.NextBatch
		}
	}
}

// dispatch translates one raw event and hands it to the engine. A non-nil
// handler error is an infrastructure failure and propagates (Supervisor
// reconnects; un-advanced events re-deliver and dedup absorbs duplicates).
// Product drops return nil.
func (c *matrixChannel) dispatch(ctx context.Context, roomID string, rawEvent json.RawMessage) error {
	msg, ok := inboundFromEvent(rawEvent, roomID, c.botID)
	if !ok {
		return nil
	}
	if !c.acceptsMedia && msg.Type != channel.MsgTypeText {
		// Media advertised but no storage to back it: leave a placeholder is
		// the resolver's job only when store != nil; without store, drop.
		return nil
	}
	if msg.Type == channel.MsgTypeUnknown || msg.Text == "" {
		return nil
	}
	// Source carries the room id and sender; the engine drops group messages
	// that are not explicitly addressed to the bot.
	if err := c.handler(ctx, msg); err != nil {
		return err
	}
	return nil
}

// sleepCtx sleeps for d or until ctx is done; false means ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ChannelDeps are the shared dependencies the Matrix Factory closes over.
type ChannelDeps struct {
	Decrypt Decrypter
	Logger  *slog.Logger
	// HTTPClient overrides the sync/send client (tests). Nil uses a default.
	HTTPClient *http.Client
	// AcceptsMedia says the deployment stores what members send (object
	// storage configured). Without it, media is treated as unsupported.
	AcceptsMedia bool
}

// RegisterMatrix registers the per-installation Matrix Factory so the
// engine.Supervisor builds + supervises one /sync loop per active Matrix
// installation. Same contract as RegisterTelegram — no engine edit.
func RegisterMatrix(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeMatrix, newMatrixFactory(deps))
}

func newMatrixFactory(deps ChannelDeps) channel.Factory {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(cfg channel.Config) (channel.Channel, error) {
		var ic installConfig
		if err := json.Unmarshal(cfg.Raw, &ic); err != nil {
			return nil, fmt.Errorf("matrix: decode installation config: %w", err)
		}
		token, err := decryptToken(ic.AccessTokenEncrypted, deps.Decrypt)
		if err != nil {
			return nil, fmt.Errorf("matrix: decrypt access token: %w", err)
		}
		if token == "" {
			return nil, errors.New("matrix: installation has no access token")
		}
		if ic.Homeserver == "" {
			return nil, errors.New("matrix: installation has no homeserver")
		}
		if _, err := parseUserID(ic.AppID); err != nil {
			return nil, err
		}
		return &matrixChannel{
			botID:        ic.AppID,
			api:          newMatrixAPI(ic.Homeserver, token, deps.HTTPClient),
			handler:      cfg.Handler,
			logger:       logger,
			acceptsMedia: deps.AcceptsMedia,
		}, nil
	}
}