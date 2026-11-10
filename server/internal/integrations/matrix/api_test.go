package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// api_test.go covers the CS-API client risks: whoami validates the token and
// returns the bot's user id on success, M_UNKNOWN_TOKEN surfaces as ErrConflict
// (the value the install flow and polling loop branch on), and sendMessage
// posts an m.room.message and returns the event id. All against an in-process
// httptest server — no live homeserver.

// fakeHomeserver implements the subset of the CS API the adapter calls.
type fakeHomeserver struct {
	t          *testing.T
	whoami     string
	whoamiCode int
	sendCode   int
	gotSend    map[string]any
	gotJoin    string
	invite     map[string]any
}

func (f *fakeHomeserver) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/account/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+"testtoken" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": "M_FORBIDDEN", "error": "bad auth"})
			return
		}
		if f.whoamiCode != 0 {
			w.WriteHeader(f.whoamiCode)
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": "M_UNKNOWN_TOKEN", "error": "Invalid token"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": f.whoami})
	})
	mux.HandleFunc("/_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"next_batch": "s123"}
		if f.invite != nil {
			resp["rooms"] = map[string]any{"invite": f.invite}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/send/m.room.message/") {
			if f.sendCode != 0 {
				w.WriteHeader(f.sendCode)
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": "M_FORBIDDEN", "error": "forbidden"})
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.gotSend = body
			_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$event_" + r.URL.Query().Get("txn")})
		} else if strings.Contains(r.URL.Path, "/join") {
			f.gotJoin = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"room_id": "!room:example.org"})
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux
}

func newFakeAPI(t *testing.T, f *fakeHomeserver) (*matrixAPI, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return newMatrixAPI(srv.URL, "testtoken", nil), srv
}

func TestWhoamiReturnsUserID(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org"}
	api, _ := newFakeAPI(t, f)
	uid, err := api.whoami(context.Background())
	if err != nil {
		t.Fatalf("whoami error: %v", err)
	}
	if uid != "@bot:example.org" {
		t.Errorf("whoami = %q, want @bot:example.org", uid)
	}
}

func TestWhoamiRejectedIsConflict(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org", whoamiCode: http.StatusForbidden}
	api, _ := newFakeAPI(t, f)
	if _, err := api.whoami(context.Background()); !isConflict(err) {
		t.Fatalf("whoami error = %v, want wrapped ErrConflict", err)
	}
}

func TestSendMessagePostsContentAndReturnsEventID(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org"}
	api, _ := newFakeAPI(t, f)
	eventID, err := api.sendMessage(context.Background(), "!room:example.org", "txn-1", map[string]any{
		"msgtype": "m.text",
		"body":    "hello",
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if eventID == "" {
		t.Fatal("sendMessage returned an empty event id")
	}
	if f.gotSend == nil || f.gotSend["msgtype"] != "m.text" || f.gotSend["body"] != "hello" {
		t.Fatalf("sendMessage did not post m.text content: %+v", f.gotSend)
	}
}

func TestSendMessageForbiddenIsNotConflict(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org", sendCode: http.StatusForbidden}
	api, _ := newFakeAPI(t, f)
	_, err := api.sendMessage(context.Background(), "!room:example.org", "txn-1", map[string]any{"msgtype": "m.text", "body": "x"})
	if err == nil {
		t.Fatal("expected an error for a forbidden send")
	}
	if isConflict(err) {
		t.Fatalf("M_FORBIDDEN must not be reported as a token conflict: %v", err)
	}
}

func TestJoinRoomPostsToRoomsJoinEndpoint(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org"}
	api, _ := newFakeAPI(t, f)
	if err := api.joinRoom(context.Background(), "!invited:example.org"); err != nil {
		t.Fatalf("joinRoom error: %v", err)
	}
	if !strings.HasPrefix(f.gotJoin, "/_matrix/client/v3/rooms/") || !strings.HasSuffix(f.gotJoin, "/join") {
		t.Fatalf("joinRoom hit unexpected path %q, want /_matrix/client/v3/rooms/{roomId}/join", f.gotJoin)
	}
}

func TestSyncParsesInvite(t *testing.T) {
	f := &fakeHomeserver{
		t:      t,
		whoami: "@bot:example.org",
		invite: map[string]any{
			"!invited:example.org": map[string]any{
				"invite_state": map[string]any{"events": []any{}},
			},
		},
	}
	api, _ := newFakeAPI(t, f)
	res, err := api.sync(context.Background(), "s0", 60000)
	if err != nil {
		t.Fatalf("sync error: %v", err)
	}
	if len(res.Rooms.Invite) != 1 {
		t.Fatalf("sync parsed %d invite rooms, want 1", len(res.Rooms.Invite))
	}
	if _, ok := res.Rooms.Invite["!invited:example.org"]; !ok {
		t.Fatalf("expected !invited:example.org to be present in rooms.invite, got %v", res.Rooms.Invite)
	}
}

func TestSyncReturnsDelta(t *testing.T) {
	f := &fakeHomeserver{t: t, whoami: "@bot:example.org"}
	api, _ := newFakeAPI(t, f)
	res, err := api.sync(context.Background(), "", 60000)
	if err != nil {
		t.Fatalf("sync error: %v", err)
	}
	if res.NextBatch != "s123" {
		t.Errorf("NextBatch = %q, want s123", res.NextBatch)
	}
}

func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrConflict.Error())
}