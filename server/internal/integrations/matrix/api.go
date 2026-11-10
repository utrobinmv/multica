package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file is the minimal Matrix client-server API surface the Phase 1
// adapter needs: whoami (install-time validation), sync (inbound long-poll),
// and send (outbound m.room.message). It is a thin hand-rolled client over
// net/http with Bearer auth — the CS API is plain JSON-over-HTTPS, so no SDK
// dependency is warranted (same decision as the Telegram adapter).

// syncTimeoutSecs is the /sync server-side hold. 60s stays under common
// proxy/LB idle timeouts while keeping the request count low.
const syncTimeoutSecs = 60

// maxSyncBodyBytes caps the /sync response we buffer. A large bot in many
// busy rooms can return more; such rooms are intentionally ignored rather than
// letting one oversized room stall the polling loop.
const maxSyncBodyBytes = 16 << 20 // 16 MiB

// ErrConflict surfaces an M_UNKNOWN_TOKEN from the homeserver: the access
// token was revoked or belongs to another device. The polling loop treats it
// as fatal for the attempt so the operator sees "bad token" instead of a
// silent reconnect loop.
var ErrConflict = errors.New("matrix: homeserver rejected the access token")

// requestError keeps a request URL out of the error string so a token can
// never become loggable through an outer error's text. Unwrap preserves
// cancellation and transport classification.
type requestError struct {
	method string
	cause  error
}

func (e *requestError) Error() string { return fmt.Sprintf("matrix: %s request failed", e.method) }

func (e *requestError) Unwrap() error { return e.cause }

// apiError is a non-2xx Matrix response.
type apiError struct {
	Code    int
	Errcode string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("matrix api: %d %s (%s)", e.Code, e.Errcode, e.Message)
}

// matrixAPI is one bot's CS-API client.
type matrixAPI struct {
	homeserver string // base URL, no trailing slash
	token      string // bearer access token
	userID     string // the bot's Matrix user id (token owner)
	client     *http.Client
}

func newMatrixAPI(homeserver, token string, client *http.Client) *matrixAPI {
	if client == nil {
		client = &http.Client{Timeout: 75 * time.Second}
	}
	return &matrixAPI{homeserver: strings.TrimRight(homeserver, "/"), token: token, client: client}
}

// whoami returns the token's owner user id (install-time validation). A
// rejected token surfaces as wrapped ErrConflict.
func (a *matrixAPI) whoami(ctx context.Context) (string, error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := a.get(ctx, "/_matrix/client/v3/account/whoami", &out); err != nil {
		return "", err
	}
	if _, err := parseUserID(out.UserID); err != nil {
		return "", fmt.Errorf("matrix whoami: %w", err)
	}
	return out.UserID, nil
}

// syncResult captures the subset of a /sync response the adapter reads.
type syncResult struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []json.RawMessage `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
		// Invite lists rooms the bot has been invited to but not yet joined.
		// The adapter accepts these (joinRoom) so a user inviting a bot to a
		// room does not have to confirm the membership manually.
		Invite map[string]json.RawMessage `json:"invite"`
	} `json:"rooms"`
}

// sync performs one /sync long-poll call and returns the delta plus the
// next_batch token to pass on the following call.
func (a *matrixAPI) sync(ctx context.Context, since string, timeoutMS int) (*syncResult, error) {
	q := make([]string, 0, 3)
	if since != "" {
		q = append(q, "since="+since)
	}
	q = append(q, "timeout="+strconv.Itoa(timeoutMS))
	path := "/_matrix/client/v3/sync?" + strings.Join(q, "&")
	var out syncResult
	if err := a.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// sendMessage posts an m.room.message event into the room. txnID is a client
// transaction id used for retry idempotency (same txnID + same content =
// exactly-once). Returns the event id.
func (a *matrixAPI) sendMessage(ctx context.Context, roomID, txnID string, content map[string]any) (string, error) {
	body, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("matrix: encode message content: %w", err)
	}
	var out struct {
		EventID string `json:"event_id"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		a.homeserver+"/_matrix/client/v3/rooms/"+urlPathEscape(roomID)+"/send/m.room.message/"+urlPathEscape(txnID),
		bytes.NewReader(body))
	if err != nil {
		return "", &requestError{method: "PUT", cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if err := a.do(req, &out); err != nil {
		return "", err
	}
	if out.EventID == "" {
		return "", errors.New("matrix: send returned no event_id")
	}
	return out.EventID, nil
}

// joinRoom accepts a room invite for the bot by POSTing a join membership.
// The method is called when /sync reports the room under rooms.invite; the
// bot cannot receive m.room.message events from a room it has only been
// invited to but has not joined.
func (a *matrixAPI) joinRoom(ctx context.Context, roomID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.homeserver+"/_matrix/client/v3/rooms/"+urlPathEscape(roomID)+"/join",
		strings.NewReader("{}"))
	if err != nil {
		return &requestError{method: "POST", cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		RoomID string `json:"room_id"`
	}
	if err := a.do(req, &out); err != nil {
		return err
	}
	return nil
}

// get performs a Bearer-authenticated GET and decodes the response into out.
func (a *matrixAPI) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.homeserver+path, nil)
	if err != nil {
		return &requestError{method: "GET", cause: err}
	}
	return a.do(req, out)
}

// do performs one prepared request, classifies the status, and decodes the
// body into out (skipped when out is nil).
func (a *matrixAPI) do(req *http.Request, out any) error {
	if a.token == "" {
		return errors.New("matrix: no access token")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return &requestError{method: req.Method, cause: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSyncBodyBytes+1))
	if err != nil {
		return fmt.Errorf("matrix: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return a.classifyError(resp.StatusCode, body)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("matrix: decode response: %w", err)
		}
	}
	return nil
}

// classifyError maps a non-2xx response to an error. An M_UNKNOWN_TOKEN — the
// homeserver's authoritative "this credential is no good" — surfaces as
// wrapped ErrConflict; everything else stays an *apiError.
func (a *matrixAPI) classifyError(code int, body []byte) error {
	var er struct {
		Errcode string `json:"errcode"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &er)
	ae := &apiError{Code: code, Errcode: er.Errcode, Message: er.Error}
	if er.Errcode == "M_UNKNOWN_TOKEN" {
		return fmt.Errorf("%w: %v", ErrConflict, ae)
	}
	return ae
}

// urlPathEscape escapes a value for use as a single URL path segment,
// preserving the Matrix ids that legitimately contain '@', ':', plus and bang.
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~', r == '@', r == ':', r == '+', r == '!', r == '/':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}