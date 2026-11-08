//go:build livetest

package matrix

// live_test.go exercises the REAL adapter code (api.go + inbound.go) against a
// live Matrix homeserver. Skipped unless invoked with -tags=livetest plus env:
//
//	MULTICA_HS=http://192.168.45.30:25387
//	MULTICA_HS_BOT_TOKEN=syt_...
//	MULTICA_HS_BOT_USERID=@bot:server
//	MULTICA_HS_ROOM=!room:server
//
// Run: go test -tags=livetest ./internal/integrations/matrix/ -run Live -v
//
// The token comes from the environment only — never hard-code it here.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func liveEnv(t *testing.T) (hs, token, userID, room string) {
	t.Helper()
	hs = os.Getenv("MULTICA_HS")
	token = os.Getenv("MULTICA_HS_BOT_TOKEN")
	userID = os.Getenv("MULTICA_HS_BOT_USERID")
	room = os.Getenv("MULTICA_HS_ROOM")
	if hs == "" || token == "" || userID == "" || room == "" {
		t.Skip("live env vars not set")
	}
	return
}

// TestLiveWhoami proves install-time credential validation against a real server.
func TestLiveWhoami(t *testing.T) {
	hs, token, userID, _ := liveEnv(t)
	api := newMatrixAPI(hs, token, nil)
	got, err := api.whoami(context.Background())
	if err != nil {
		t.Fatalf("whoami error: %v", err)
	}
	if got != userID {
		t.Fatalf("whoami = %q, want %q", got, userID)
	}
}

// TestLiveSyncInbound reads a live /sync delta and confirms the adapter
// translates an addressed-to-bot m.room.message into an InboundMessage.
func TestLiveSyncInbound(t *testing.T) {
	hs, token, userID, room := liveEnv(t)
	api := newMatrixAPI(hs, token, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := api.sync(ctx, "", 3000)
	if err != nil {
		t.Fatalf("sync error: %v", err)
	}

	found := false
	for _, events := range res.Rooms.Join {
		for _, raw := range events.Timeline.Events {
			var ev roomEvent
			if err := json.Unmarshal(raw, &ev); err != nil || ev.Type != "m.room.message" {
				continue
			}
			msg, ok := inboundFromEvent(raw, room, userID)
			if !ok {
				continue
			}
			if msg.AddressedToBot {
				found = true
				if msg.Source.ChatID != room {
					t.Errorf("ChatID = %q, want %q", msg.Source.ChatID, room)
				}
				if !strings.Contains(msg.Text, userID) {
					t.Errorf("inbound text %q did not contain bot user id %q", msg.Text, userID)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no addressed-to-bot m.room.message in the live sync delta for %q", userID)
	}
}

// TestLiveSendMessage posts a reply through the real client and reads back the
// event id (proves the outbound send path against the live server).
func TestLiveSendMessage(t *testing.T) {
	hs, token, _, room := liveEnv(t)
	api := newMatrixAPI(hs, token, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	evID, err := api.sendMessage(ctx, room, "txn-multica-"+time.Now().Format("150405"), map[string]any{
		"msgtype": "m.text",
		"body":    "multica matrix adapter live test: reply delivered",
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if evID == "" {
		t.Fatal("empty event id from sendMessage")
	}
	t.Logf("sent event_id=%s", evID)
}