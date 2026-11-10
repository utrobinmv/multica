package matrix

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// inbound_test.go covers the event->InboundMessage translation risks: only
// m.room.message events are actionable, the bot's own echo is dropped, an
// @mention or m.mentions marks the message addressed, media msgtypes map onto
// the normalized set, and the dedup key is the stable event id.

func mustRaw(t *testing.T, src string) []byte {
	t.Helper()
	return []byte(src)
}

func mk(text string) map[string]any {
	return map[string]any{"msgtype": "m.text", "body": text}
}

func TestInboundFromEventOnlyRoomMessage(t *testing.T) {
	// A membership (state) event is not actionable.
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.member","sender":"@alice:example.org","content":{"membership":"join"}}`)
	if _, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org"); ok {
		t.Fatal("expected a m.room.member event to be dropped")
	}
}

func TestInboundFromEventDropsOwnEcho(t *testing.T) {
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@bot:example.org","content":{"msgtype":"m.text","body":"hi"}}`)
	if _, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org"); ok {
		t.Fatal("expected the bot's own echo to be dropped")
	}
}

func TestInboundFromEventTextWithMentionIsAddressed(t *testing.T) {
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"hello @bot:example.org can you help","m.mentions":{"user_ids":["@bot:example.org"]}}}`)
	msg, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org")
	if !ok {
		t.Fatal("expected an addressed m.room.message to be actionable")
	}
	if !msg.AddressedToBot {
		t.Error("expected AddressedToBot=true for a message mentioning the bot")
	}
	if msg.Source.ChatID != "!room:example.org" {
		t.Errorf("ChatID = %q, want room id", msg.Source.ChatID)
	}
	if msg.Source.SenderID != "@alice:example.org" {
		t.Errorf("SenderID = %q, want @alice:example.org", msg.Source.SenderID)
	}
	if msg.MessageID != "$e1" || msg.EventID != "$e1" {
		t.Errorf("MessageID=%q EventID=%q, want $e1", msg.MessageID, msg.EventID)
	}
	if msg.Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("ChatType = %q, want group", msg.Source.ChatType)
	}
}

func TestInboundFromEventRawCarriesBotID(t *testing.T) {
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"@bot hello"}}`)
	msg, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org")
	if !ok {
		t.Fatal("expected actionable")
	}
	decoded, err := decodeMatrixRaw(msg)
	if err != nil {
		t.Fatalf("decodeMatrixRaw error: %v", err)
	}
	if decoded.BotID != "@bot:example.org" {
		t.Errorf("raw.BotID = %q, want @bot:example.org", decoded.BotID)
	}
}

func TestInboundFromEventCasualMessageNowAddressed(t *testing.T) {
	// A plain room message with no mentions is treated as addressed to the bot:
	// users chat with a single bot in a room without typing the full @...:id
	// handle every time. Messages that name ANOTHER participant (mentions
	// present but not the bot) still count as unaddressed so the bot never
	// dogs another user's conversation.
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"casual chat"}}`)
	msg, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org")
	if !ok {
		t.Fatal("expected actionable")
	}
	if !msg.AddressedToBot {
		t.Error("expected AddressedToBot=true for a plain message with no mentions (bot replies without a @handle prefix)")
	}

	rawAlt := mustRaw(t, `{"event_id":"$e2","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"hey @bob:example.org what do you think","m.mentions":{"user_ids":["@bob:example.org"]}}}`)
	msgAlt, ok := inboundFromEvent(rawAlt, "!room:example.org", "@bot:example.org")
	if !ok {
		t.Fatal("expected actionable")
	}
	if msgAlt.AddressedToBot {
		t.Error("expected AddressedToBot=false when the message mentions another participant, not the bot")
	}
}

func TestInboundFromEventFlattensFormatting(t *testing.T) {
	raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"m.text","body":"hi","format":"org.matrix.custom.html","formatted_body":"<b>hi</b> there"}}`)
	msg, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org")
	if !ok {
		t.Fatal("expected actionable")
	}
	if msg.Text != "hi there" {
		t.Errorf("Text = %q, want 'hi there' after stripping HTML", msg.Text)
	}
}

func TestInboundFromEventMapsMediaTypes(t *testing.T) {
	cases := []struct {
		msgtype string
		want    channel.MsgType
	}{
		{"m.image", channel.MsgTypeImage},
		{"m.file", channel.MsgTypeFile},
		{"m.audio", channel.MsgTypeAudio},
		{"m.video", channel.MsgTypeVideo},
		{"m.sticker", channel.MsgTypeUnknown},
	}
	for _, c := range cases {
		raw := mustRaw(t, `{"event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","content":{"msgtype":"`+c.msgtype+`","body":"x","url":"mxc://hs/abc"}}`)
		msg, ok := inboundFromEvent(raw, "!room:example.org", "@bot:example.org")
		if !ok {
			t.Fatalf("msgtype %s: expected actionable", c.msgtype)
		}
		if msg.Type != c.want {
			t.Errorf("msgtype %s -> Type %q, want %q", c.msgtype, msg.Type, c.want)
		}
	}
}