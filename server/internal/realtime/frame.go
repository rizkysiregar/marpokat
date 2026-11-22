// Package realtime is the WebSocket side of Marpokat (PLAN M3). The contract is spec/events.md.
//
// Events travel like this: a service commits a change and tells the Publisher, which asks the database who must hear
// of it and PUBLISHes the event to one Redis topic per receiving user. Every pod subscribes to the topics of the
// users connected to it (Hub) and writes the event to their sockets. Delivery is best effort; clients repair gaps
// with sync, which is why nothing here needs to be durable.
package realtime

import (
	"encoding/json"
	"time"

	"github.com/coder/websocket"
)

// ProtocolVersion is the only version of the WebSocket protocol this server speaks.
const ProtocolVersion = 1

// Client to server.
const (
	typeAuth        = "auth"
	typePing        = "ping"
	typeMessageSend = "message.send"
	typeTypingStart = "typing.start"
	typeTypingStop  = "typing.stop"
	typeReadMark    = "read.mark"
)

// Server to client.
const (
	typeAuthOK       = "auth.ok"
	typePong         = "pong"
	typeAck          = "ack"
	typeError        = "error"
	EvMessageNew     = "message.new"
	EvReadUpdated    = "read.updated"
	EvTyping         = "typing"
	EvPresence       = "presence"
	EvChannelAdded   = "channel.added"
	EvChannelUpdated = "channel.updated"
	EvChannelRemoved = "channel.removed"
	EvMemberAdded    = "member.added"
	EvMemberRemoved  = "member.removed"
)

// Close codes (spec/events.md, section 9).
const (
	CloseServerRestart      = websocket.StatusServiceRestart // 1012: reconnect at once
	CloseTryAgainLater      = websocket.StatusTryAgainLater  // 1013: the client was too slow to keep up
	CloseProtocolError      = websocket.StatusCode(4000)
	CloseAuthTimeout        = websocket.StatusCode(4001)
	CloseAuthFailed         = websocket.StatusCode(4003)
	CloseTokenExpired       = websocket.StatusCode(4004)
	CloseUnsupportedVersion = websocket.StatusCode(4005)
	CloseHeartbeatTimeout   = websocket.StatusCode(4008)
	CloseRateLimited        = websocket.StatusCode(4029)
)

// inFrame is any frame from a client: {"type", "id"?, "data"}.
type inFrame struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type outFrame struct {
	Type string    `json:"type"`
	ID   string    `json:"id,omitempty"`
	TS   time.Time `json:"ts"`
	Data any       `json:"data"`
}

// encode builds a frame for a client. data may be json.RawMessage (an event that came through Redis).
func encode(typ, id string, data any, now time.Time) []byte {
	if data == nil {
		data = struct{}{}
	}
	b, err := json.Marshal(outFrame{Type: typ, ID: id, TS: now.UTC(), Data: data})
	if err != nil { // only possible for a value that cannot be JSON, a bug of the caller
		b, _ = json.Marshal(outFrame{Type: typeError, ID: id, TS: now.UTC(), Data: errorData{Code: "internal_error", Message: "cannot encode frame"}})
	}
	return b
}

type errorData struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// wire is an event on its way through Redis: the connection that caused it (which already knows), the type, the data.
type wire struct {
	Origin string          `json:"o,omitempty"`
	Type   string          `json:"t"`
	Data   json.RawMessage `json:"d"`
}
