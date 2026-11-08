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
		_ = json.NewEncoder(w).Encode(map[string]any{"next_batch": "s123"})
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