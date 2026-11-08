// Package matrix is the Matrix integration for the channel-agnostic engine.
// It follows the Telegram BYO model: the workspace admin creates a bot account
// on a Matrix homeserver, pastes the homeserver base URL + a fresh access
// token into Multica, and the installation is keyed by the bot's Matrix user
// id (the token's owner). Inbound runs on a per-installation /sync long-poll
// loop (matrix_channel.go) — Matrix offers no push transport over plain CS
// API, and long polling is the deployment model that needs no public HTTPS
// endpoint, matching the Telegram/Feishu/Slack "persistent per-installation
// connection supervised by engine.Supervisor" shape. Outbound agent replies
// are sent via PUT /rooms/{room}/send/m.room.message (outbound.go).
//
// Phase 1 scope: UNENCRYPTED rooms only. A plain CS-API /sync client cannot
// read m.room.encrypted (Olm/Megolm) event payloads, so encrypted rooms are
// not supported. The homeserver's /sync delta is filtered to m.room.message.
package matrix

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// TypeMatrix is the channel discriminator for the Matrix adapter. Defined
// here (not in the channel core) so registering the platform never edits the
// core, mirroring TypeTelegram.
const TypeMatrix channel.Type = "matrix"

// installConfig is the JSON shape stored in channel_installation.config for a
// Matrix installation.
//
// app_id holds the bot's Matrix user id (e.g. "@multica_bot:example.org"),
// which fills the generic (channel_type, config->>'app_id') routing slot and
// the unique index guarantees one bot maps to one agent across all workspaces.
//
// access_token_encrypted is base64-encoded secretbox ciphertext, never
// plaintext, mirroring Telegram's bot_token_encrypted and Feishu's
// app_secret_encrypted.
type installConfig struct {
	AppID                string `json:"app_id"`
	Homeserver           string `json:"homeserver"`
	AccessTokenEncrypted string `json:"access_token_encrypted"`
}

// credentials is the decoded, decrypted form the transports run on. The
// installation identity (workspace / agent / installer) is deliberately
// absent: it is resolved per message by the Router, as in Feishu and Slack.
type credentials struct {
	UserID      string
	Homeserver  string
	AccessToken string
}

// Decrypter turns stored ciphertext into plaintext. The wiring injects a
// secretbox-backed implementation; tests inject nil (stored bytes are treated
// as plaintext).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials parses the per-installation config blob and decrypts the
// stored access token. It is the single place the Matrix config JSON is
// interpreted.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	if len(raw) == 0 {
		return credentials{}, errors.New("matrix: empty installation config")
	}
	var cfg installConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return credentials{}, fmt.Errorf("decode matrix installation config: %w", err)
	}
	token, err := decryptToken(cfg.AccessTokenEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("decrypt access token: %w", err)
	}
	return credentials{
		UserID:      cfg.AppID,
		Homeserver:  cfg.Homeserver,
		AccessToken: token,
	}, nil
}

// PublicConfig is the non-secret subset of an installation config, safe to
// surface on the management API (the encrypted access token is never
// included).
type PublicConfig struct {
	UserID     string
	Homeserver string
}

// DecodePublicConfig extracts the display-safe fields from a stored config
// blob. A decode miss yields a zero PublicConfig rather than an error so the
// management list still renders the row.
func DecodePublicConfig(raw json.RawMessage) PublicConfig {
	var cfg installConfig
	_ = json.Unmarshal(raw, &cfg)
	return PublicConfig{UserID: cfg.AppID, Homeserver: cfg.Homeserver}
}

// decryptToken base64-decodes the stored ciphertext (tolerating MIME newline
// wrapping) and runs it through the injected Decrypter; mirrors the Telegram
// helper of the same name.
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stripWhitespace(enc))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseUserID validates the shape of a Matrix user id ("@localpart:servername")
// and returns it normalized. Used both on install (the homeserver's whoami
// verdict must carry a user id) and on inbound routing.
func parseUserID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "@") || !strings.Contains(id, ":") {
		return "", errors.New("matrix: invalid user id (must look like @name:server)")
	}
	return id, nil
}