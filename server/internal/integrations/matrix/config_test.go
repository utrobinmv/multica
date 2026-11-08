package matrix

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// These tests cover the config layer risk: the at-rest ciphertext of the
// access token must round-trip through decodeCredentials and must never
// surface through DecryptPublicConfig (the management API surface).

func TestDecodeCredentialsRoundTripsSealedToken(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:                "@bot:example.org",
		Homeserver:           "https://matrix.example.org",
		AccessTokenEncrypted: tokenFixture(t, "syt_secret_token_example"),
	})
	creds, err := decodeCredentials(raw, nil) // nil decrypt => stored bytes are plaintext
	if err != nil {
		t.Fatalf("decodeCredentials returned error: %v", err)
	}
	if creds.UserID != "@bot:example.org" {
		t.Errorf("UserID = %q, want @bot:example.org", creds.UserID)
	}
	if creds.Homeserver != "https://matrix.example.org" {
		t.Errorf("Homeserver = %q, want https://matrix.example.org", creds.Homeserver)
	}
	if creds.AccessToken != "syt_secret_token_example" {
		t.Errorf("AccessToken = %q, want the sealed token back", creds.AccessToken)
	}
}

func TestDecodeCredentialsEmptyConfigIsError(t *testing.T) {
	if _, err := decodeCredentials(nil, nil); err == nil {
		t.Fatal("expected an error for an empty config")
	}
}

func TestDecodePublicConfigNeverLeaksToken(t *testing.T) {
	raw, _ := json.Marshal(installConfig{
		AppID:                "@bot:example.org",
		Homeserver:           "https://matrix.example.org",
		AccessTokenEncrypted: tokenFixture(t, "syt_MUST_NOT_LEAK"),
	})
	pub := DecodePublicConfig(raw)
	b, _ := json.Marshal(pub)
	if containsString(string(b), "syt_MUST_NOT_LEAK") || containsString(string(b), "must_not_leak") {
		t.Fatalf("DecodePublicConfig exposed the token: %s", b)
	}
	if pub.UserID != "@bot:example.org" {
		t.Errorf("UserID = %q, want @bot:example.org", pub.UserID)
	}
}

func TestDecodePublicConfigDecodeMissIsZero(t *testing.T) {
	pub := DecodePublicConfig([]byte(`{not json`))
	if pub.UserID != "" || pub.Homeserver != "" {
		t.Fatalf("expected zero PublicConfig on decode miss, got %+v", pub)
	}
}

func TestParseUserIDRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "bot:example.org", "@bot"} {
		if _, err := parseUserID(bad); err == nil {
			t.Errorf("parseUserID(%q) expected an error", bad)
		}
	}
	// A server name may itself contain a colon (e.g. @bot:example.org:8448).
	if id, err := parseUserID("  @bot:example.org  "); err != nil || id != "@bot:example.org" {
		t.Errorf("parseUserID valid = %q, %v; want @bot:example.org", id, err)
	}
}

func tokenFixture(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}