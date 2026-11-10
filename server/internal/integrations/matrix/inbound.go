package matrix

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// This file translates one Matrix m.room.message event into a normalized
// channel.InboundMessage. Matrix room state and message events share one
// envelope ({event_id, type, sender, content, ...}), so the adapter reads
// exactly the fields it needs and leaves everything else in Raw.

// roomEvent is the raw CS-API event envelope. Phase 1 only handles
// m.room.message events; edits (m.replace) and reactions are out of scope.
type roomEvent struct {
	EventID string          `json:"event_id"`
	Type    string          `json:"type"`
	Sender  string          `json:"sender"`
	Content json.RawMessage `json:"content"`
	// RoomID is stamped in by dispatch from the sync delta (the event payload
	// itself does not carry it).
	RoomID string `json:"room_id,omitempty"`
}

// matrixRawEvent is the adapter's own wrapper persisted into
// InboundMessage.Raw. It carries the bot user id so the installation resolver
// can route the message to its installation via config->>'app_id' (the generic
// (channel_type, app_id) routing slot), mirroring telegramRawEvent.BotID. The
// original event stays untouched under Event.
type matrixRawEvent struct {
	BotID string          `json:"bot_id"`
	Event json.RawMessage `json:"event"`
}

// messageContent is the body of an m.room.message event.
type messageContent struct {
	MsgType   string           `json:"msgtype"`
	Body      string           `json:"body"`
	Format    string           `json:"format"`
	Formatted string           `json:"formatted_body"`
	Mentions  *mentionsContent `json:"m.mentions,omitempty"`
	// URL and Info carry mxc:// media references (image/file/audio/video) for
	// the Phase 1 media ingest path.
	URL  string `json:"url"`
	Info struct {
		Mimetype string `json:"mimetype"`
		Size     int64  `json:"size"`
	} `json:"info"`
	Filename string `json:"filename,omitempty"`
}

type mentionsContent struct {
	UserIDs []string `json:"user_ids"`
}

// inboundFromEvent converts one raw CS-API event to a channel message. ok is
// false when the event is not an actionable m.room.message (state events,
// membership, reactions, edits, the bot's own echo). Source.ChatID is the
// room id (from the sync delta), SenderID the event's sender.
func inboundFromEvent(raw json.RawMessage, roomID, botUserID string) (channel.InboundMessage, bool) {
	var ev roomEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return channel.InboundMessage{}, false
	}
	if ev.Type != "m.room.message" {
		return channel.InboundMessage{}, false
	}
	// Ignore the bot's own echo — /sync includes our own sent events.
	if ev.Sender == botUserID {
		return channel.InboundMessage{}, false
	}
	var content messageContent
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return channel.InboundMessage{}, false
	}

	// Route + inbound carry the bot user id so the installation resolver and
	// the auditor can identify the installation without extra lookups.
	rawStamped, _ := json.Marshal(matrixRawEvent{BotID: botUserID, Event: raw})

	msg := channel.InboundMessage{
		EventID:        ev.EventID,
		MessageID:      ev.EventID, // dedup keys on the stable event id
		Type:           mapMatrixMsgType(content.MsgType),
		AddressedToBot: addressedToBot(content, botUserID),
		Source: channel.Source{
			ChannelType: TypeMatrix,
			ChatType:    channel.ChatTypeGroup, // Phase 1: rooms are group chats
			ChatID:      roomID,
			SenderID:    ev.Sender,
		},
		Raw: rawStamped,
	}

	// Text is flattened from the formatted (HTML) body when present, else the
	// plain body. Media messages carry their body as a caption/placeholder.
	if content.Formatted != "" && content.Format == "org.matrix.custom.html" {
		msg.Text = stripHTML(content.Formatted)
	} else {
		msg.Text = content.Body
	}
	msg.CommandText = msg.Text
	return msg, true
}

// decodeMatrixRaw unwraps the adapter's Raw envelope so the ResolverSet and
// auditor can read the bot user id / event.
func decodeMatrixRaw(msg channel.InboundMessage) (matrixRawEvent, error) {
	var raw matrixRawEvent
	if len(msg.Raw) == 0 {
		return matrixRawEvent{}, errors.New("matrix: inbound message Raw is empty")
	}
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		return matrixRawEvent{}, fmt.Errorf("decode matrix inbound raw: %w", err)
	}
	return raw, nil
}

// mapMatrixMsgType translates a Matrix msgtype onto the normalized set.
func mapMatrixMsgType(mt string) channel.MsgType {
	switch mt {
	case "m.text", "m.notice", "m.emote":
		return channel.MsgTypeText
	case "m.image":
		return channel.MsgTypeImage
	case "m.file":
		return channel.MsgTypeFile
	case "m.audio":
		return channel.MsgTypeAudio
	case "m.video":
		return channel.MsgTypeVideo
	default:
		return channel.MsgTypeUnknown
	}
}

// addressedToBot reports whether a room message is an interaction with the
// bot. True when the bot appears in Matrix m.mentions or in the body — or when
// the message names NOBODY (no m.mentions at all), because users chat with a
// single bot in a room without typing the full @...:id handle every time, and
// the engine drops otherwise. A message that mentions another participant
// (mentions present but not the bot) is NOT addressed, so the bot never dogs
// another user's conversation. Matrix room replies carry only an event id,
// whose owner the adapter cannot resolve here, so a reply counts only when the
// body also mentions the bot.
func addressedToBot(content messageContent, botUserID string) bool {
	// No mentions at all: a plain room message not tied to any participant.
	// Treat it as addressed so a bot answers without the user typing the full
	// @handle every time.
	if content.Mentions == nil {
		return true
	}
	// An explicit empty mentions block means nothing was addressed to anyone —
	// same treatment as a plain message.
	if len(content.Mentions.UserIDs) == 0 {
		return true
	}
	for _, u := range content.Mentions.UserIDs {
		if u == botUserID {
			return true
		}
	}
	// Mentions present but none target this bot: not addressed, so the bot does
	// not dog another participant's conversation.
	return false
}

// stripHTML removes the tags from a Matrix formatted (HTML) body, collapsing
// whitespace, so the agent reads plain text. Links become "text (url)".
func stripHTML(formatted string) string {
	var b strings.Builder
	inTag := false
	depth := 0
	for i := 0; i < len(formatted); i++ {
		c := formatted[i]
		switch {
		case c == '<':
			inTag = true
			if i+1 < len(formatted) && formatted[i+1] == '/' {
				depth--
			} else {
				depth++
			}
			continue
		case c == '>':
			inTag = false
			continue
		case inTag:
			continue
		case depth > 0:
			// text inside a tag with depth (e.g. <a href>text</a>) still emits
			// its text — but Phase 1 treats links as plain text.
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}