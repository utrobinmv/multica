package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// matrix_channel_test.go covers the /sync dispatch translation, specifically
// that an @mention addressed to the bot is handed to the handler as an
// addressed message using the bot id from the installation config (c.botID),
// not the client's (unpopulated) userID field.

func TestDispatchHandlesMentionedMessage(t *testing.T) {
	// botID is the real installation config's app_id. The API client's userID
	// field is intentionally left empty by newMatrixAPI (matches production:
	// it is only ever set after a whoami that dispatch never runs).
	c := &matrixChannel{
		botID:        "@atlas:example.org",
		api:          newMatrixAPI("http://hs", "tok", testClient()),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		acceptsMedia: true,
	}

	handled := make(chan channel.InboundMessage, 1)
	c.handler = func(_ context.Context, m channel.InboundMessage) error {
		handled <- m
		return nil
	}

	raw := json.RawMessage(`{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"hi @atlas:example.org","m.mentions":{"user_ids":["@atlas:example.org"]}}}`)
	if err := c.dispatch(context.Background(), "!room:example.org", raw); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}

	select {
	case m := <-handled:
		if !m.AddressedToBot {
			t.Fatal("expected AddressedToBot=true for a message mentioning the bot by its config user id")
		}
		dec, err := decodeMatrixRaw(m)
		if err != nil {
			t.Fatalf("decode raw: %v", err)
		}
		if dec.BotID != "@atlas:example.org" {
			t.Errorf("raw.BotID = %q, want the installation's config user id (botID), not the empty client userID", dec.BotID)
		}
	default:
		t.Fatal("expected the handler to be invoked for an addressed message")
	}
}

func TestDispatchPassesUnaddressedRoomMessageAsNotAddressed(t *testing.T) {
	// dispatch hands ANY m.room.message to the handler; the engine drops group
	// messages that are not addressed to the bot. dispatch's own contract is
	// to preserve AddressedToBot=false on the translated message so the engine
	// can make that drop decision downstream.
	c := &matrixChannel{
		botID:        "@atlas:example.org",
		api:          newMatrixAPI("http://hs", "tok", testClient()),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		acceptsMedia: true,
	}
	handled := make(chan channel.InboundMessage, 1)
	c.handler = func(_ context.Context, m channel.InboundMessage) error {
		handled <- m
		return nil
	}
	raw := json.RawMessage(`{"event_id":"$e2","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"casual chat"}}`)
	if err := c.dispatch(context.Background(), "!room:example.org", raw); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	select {
	case m := <-handled:
		if m.AddressedToBot {
			t.Fatal("expected AddressedToBot=false for a group message that does not mention the bot")
		}
	default:
		t.Fatal("expected dispatch to forward the m.room.message to the handler")
	}
}

// testClient returns a no-op HTTP client whose Do never gets a real request in
// these tests (dispatch does not call the homeserver).
func testClient() *http.Client {
	dial := func(network, addr string) (io.Closer, error) { return nil, errors.New("no network") }
	_ = dial
	return &http.Client{Transport: &transportStub{}}
}

type transportStub struct {
	sync.Once
}

func (t *transportStub) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected network call in unit test")
}

var _ = strings.TrimSpace // keep strings import if referenced