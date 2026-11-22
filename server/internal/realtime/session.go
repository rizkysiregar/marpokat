package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// Session is one WebSocket connection of one user.
//
// Frames from the client are handled one at a time on the goroutine that reads them, so a client's requests are
// answered in the order it sent them. Everything for the client (acks, errors, events) goes through one buffered
// queue to a writer goroutine; a client too slow to drain it is closed (1013) and catches up with sync.
type Session struct {
	h    *Handler
	conn *websocket.Conn
	id   string
	ctx  context.Context
	// cancel ends ctx when the session is over; it never interrupts a Read, closing the connection does.
	cancel context.CancelFunc

	tenant tenant.Tenant
	userID uuid.UUID
	ext    string

	out     chan []byte
	limiter *rate.Limiter

	closeOnce sync.Once
	closing   atomic.Bool
	closeCode atomic.Int64
	closeDone chan struct{}

	registered bool
	online     bool
	tokenTimer *time.Timer

	mu     sync.Mutex
	typing map[uuid.UUID]*typingState
}

type typingState struct {
	timer     *time.Timer
	lastStart time.Time
}

func newSession(h *Handler, conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc) *Session {
	return &Session{
		h:         h,
		conn:      conn,
		id:        uuid.NewString(),
		ctx:       ctx,
		cancel:    cancel,
		out:       make(chan []byte, h.opts.SendBuffer),
		limiter:   rate.NewLimiter(rate.Limit(h.opts.RequestsPerSecond), h.opts.RequestsPerSecond),
		closeDone: make(chan struct{}),
		typing:    map[uuid.UUID]*typingState{},
	}
}

// run serves the connection until it ends.
func (s *Session) run() {
	defer s.cancel()
	if m := s.h.metrics; m != nil {
		m.WSConnections.Inc()
		defer m.WSConnections.Dec()
	}
	authTimer := time.AfterFunc(s.h.opts.AuthTimeout, func() {
		s.requestClose(CloseAuthTimeout, "no auth frame within the timeout")
	})
	ok := s.authenticate()
	authTimer.Stop()
	if ok {
		go s.writeLoop()
		s.readLoop()
	}
	s.teardown()
}

// requestClose ends the connection with a close frame. Safe to call from any goroutine, any number of times.
func (s *Session) requestClose(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		s.closeCode.Store(int64(code))
		if len(reason) > 120 {
			reason = reason[:120]
		}
		go func() {
			_ = s.conn.Close(code, reason)
			close(s.closeDone)
		}()
	})
}

// enqueue queues a frame for the client without ever blocking the caller.
func (s *Session) enqueue(frame []byte) {
	select {
	case s.out <- frame:
	default:
		s.requestClose(CloseTryAgainLater, "too slow to keep up, reconnect and sync")
	}
}

func (s *Session) write(frame []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.h.opts.WriteTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

func (s *Session) writeLoop() {
	for {
		select {
		case frame := <-s.out:
			if err := s.write(frame); err != nil {
				s.requestClose(websocket.StatusInternalError, "cannot write")
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// reject answers a failed handshake with an error frame and ends the connection.
func (s *Session) reject(code websocket.StatusCode, id, errCode, msg string) {
	_ = s.write(encode(typeError, id, errorData{Code: errCode, Message: msg}, time.Now()))
	s.requestClose(code, msg)
}

type authData struct {
	APIKey          string `json:"api_key"`
	Token           string `json:"token"`
	ProtocolVersion int    `json:"protocol_version"`
}

// authenticate reads the first frame, checks it and registers the session. It reports whether the session is live.
func (s *Session) authenticate() bool {
	h := s.h
	_, data, err := s.conn.Read(s.ctx)
	if err != nil {
		return false
	}
	var f inFrame
	if json.Unmarshal(data, &f) != nil || f.Type != typeAuth {
		s.reject(CloseProtocolError, f.ID, httpx.CodeBadRequest, "the first frame must be auth")
		return false
	}
	var a authData
	if json.Unmarshal(f.Data, &a) != nil {
		s.reject(CloseProtocolError, f.ID, httpx.CodeBadRequest, "auth data must be a JSON object")
		return false
	}
	if a.ProtocolVersion != ProtocolVersion {
		s.reject(CloseUnsupportedVersion, f.ID, httpx.CodeBadRequest,
			fmt.Sprintf("protocol_version %d is not supported, use %d", a.ProtocolVersion, ProtocolVersion))
		return false
	}

	t, secret, err := h.tenants.Resolve(s.ctx, strings.TrimSpace(a.APIKey))
	if errors.Is(err, tenant.ErrNotFound) {
		s.reject(CloseAuthFailed, f.ID, httpx.CodeUnauthenticated, "unknown or disabled api key")
		return false
	}
	if err != nil {
		h.log.Error("websocket auth: tenant lookup failed", "error", err)
		s.reject(websocket.StatusInternalError, f.ID, httpx.CodeUnavailable, "authentication temporarily unavailable")
		return false
	}
	ext, exp, err := auth.VerifyUserTokenExpiry(a.Token, secret, h.now())
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		s.reject(CloseTokenExpired, f.ID, httpx.CodeTokenExpired, "token expired")
		return false
	case err != nil:
		s.reject(CloseAuthFailed, f.ID, httpx.CodeUnauthenticated, err.Error())
		return false
	}
	userID, err := h.users(s.ctx, t.ID, ext)
	if errors.Is(err, auth.ErrUnknownUser) {
		s.reject(CloseAuthFailed, f.ID, httpx.CodeUnauthenticated, "user "+ext+" does not exist in this app; its backend must create it first")
		return false
	}
	if err != nil {
		h.log.Error("websocket auth: user lookup failed", "error", err)
		s.reject(websocket.StatusInternalError, f.ID, httpx.CodeUnavailable, "authentication temporarily unavailable")
		return false
	}
	u, err := h.userSvc.Get(s.ctx, t.ID, ext)
	if err != nil {
		h.log.Error("websocket auth: load user failed", "error", err)
		s.reject(websocket.StatusInternalError, f.ID, httpx.CodeUnavailable, "authentication temporarily unavailable")
		return false
	}
	s.tenant, s.userID, s.ext = t, userID, ext

	first, ok, err := h.presence.Connect(s.ctx, t.ID, userID, s.id, h.opts.MaxSessionsPerUser)
	if err != nil {
		h.log.Error("websocket auth: presence failed", "error", err)
		s.reject(websocket.StatusInternalError, f.ID, httpx.CodeUnavailable, "realtime temporarily unavailable")
		return false
	}
	if !ok {
		s.reject(CloseRateLimited, f.ID, httpx.CodeRateLimited, "too many connections for this user")
		return false
	}
	s.online = true
	// registered before auth.ok goes out, so no event can fall between the answer and the subscription
	if err := h.hub.Register(s.ctx, s); err != nil {
		code := websocket.StatusInternalError
		if errors.Is(err, errHubClosed) {
			code = CloseServerRestart
		}
		s.reject(code, f.ID, httpx.CodeUnavailable, "realtime temporarily unavailable")
		return false
	}
	s.registered = true

	hello := encode(typeAuthOK, f.ID, map[string]any{
		"user":                  u,
		"connection_id":         s.id,
		"server_time":           time.Now().UTC(),
		"heartbeat_interval_ms": h.opts.HeartbeatInterval.Milliseconds(),
	}, time.Now())
	if err := s.write(hello); err != nil {
		return false
	}
	// the connection ends by itself when the token does: tokens are only checked at auth otherwise
	s.tokenTimer = time.AfterFunc(max(time.Until(exp), 0), func() {
		_ = s.write(encode(typeError, "", errorData{Code: httpx.CodeTokenExpired, Message: "token expired"}, time.Now()))
		s.requestClose(CloseTokenExpired, "token expired")
	})
	if first {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			h.pub.PresenceChanged(ctx, t.ID, userID, ext, true, nil)
		}()
	}
	return true
}

func (s *Session) readLoop() {
	idle := time.AfterFunc(s.h.opts.IdleTimeout, func() {
		s.requestClose(CloseHeartbeatTimeout, "no frame within the timeout")
	})
	defer idle.Stop()
	for {
		typ, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return
		}
		idle.Reset(s.h.opts.IdleTimeout)
		if typ != websocket.MessageText {
			s.fail("", httpx.NewError(400, httpx.CodeBadRequest, "binary frames are not supported"))
			continue
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil || f.Type == "" {
			s.fail("", httpx.NewError(400, httpx.CodeBadRequest, "a frame is a JSON object with a type"))
			continue
		}
		s.handle(f)
	}
}

func (s *Session) handle(f inFrame) {
	if m := s.h.metrics; m != nil {
		m.WSFramesIn.WithLabelValues(knownType(f.Type)).Inc()
	}
	if f.Type == typePing { // never limited: it is what keeps the connection alive
		s.onPing()
		return
	}
	if !s.limiter.Allow() {
		s.fail(f.ID, httpx.ErrRateLimited(time.Second/time.Duration(s.h.opts.RequestsPerSecond), "too many requests on this connection"))
		return
	}
	switch f.Type {
	case typeMessageSend:
		s.onSend(f)
	case typeTypingStart:
		s.onTyping(f, true)
	case typeTypingStop:
		s.onTyping(f, false)
	case typeReadMark:
		s.onRead(f)
	case typeAuth:
		s.fail(f.ID, httpx.NewError(400, httpx.CodeBadRequest, "this connection is already authenticated"))
	default:
		s.fail(f.ID, httpx.NewError(400, httpx.CodeBadRequest, "unknown frame type %q", f.Type))
	}
}

func knownType(t string) string {
	switch t {
	case typePing, typeMessageSend, typeTypingStart, typeTypingStop, typeReadMark, typeAuth:
		return t
	}
	return "unknown"
}

func (s *Session) onPing() {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	if err := s.h.presence.Touch(ctx, s.tenant.ID, s.userID, s.id); err != nil {
		s.h.log.Warn("presence not refreshed", "error", err)
	}
	s.enqueue(encode(typePong, "", nil, time.Now()))
}

func (s *Session) onSend(f inFrame) {
	if f.ID == "" {
		s.fail("", httpx.FieldErrors{"id": "is required on message.send, it is what the ack answers"})
		return
	}
	var d struct {
		ChannelID     string          `json:"channel_id"`
		ClientMsgID   string          `json:"client_msg_id"`
		Text          string          `json:"text"`
		AttachmentIDs []string        `json:"attachment_ids"`
		ReplyToID     json.RawMessage `json:"reply_to_id"`
	}
	if json.Unmarshal(f.Data, &d) != nil {
		s.fail(f.ID, httpx.NewError(400, httpx.CodeBadRequest, "data must be a JSON object"))
		return
	}
	channelID, err := uuid.Parse(d.ChannelID)
	if err != nil {
		s.fail(f.ID, httpx.ErrNotFound("channel not found"))
		return
	}
	attachmentIDs, replyTo, err := message.ParseSendRefs(d.AttachmentIDs, d.ReplyToID)
	if err != nil {
		s.fail(f.ID, err)
		return
	}
	msg, _, err := s.h.msgs.Send(s.ctx, message.SendInput{
		TenantID: s.tenant.ID, ChannelID: channelID, SenderID: s.userID,
		ClientMsgID: d.ClientMsgID, Text: d.Text, AttachmentIDs: attachmentIDs, ReplyToID: replyTo, Origin: s.id,
	})
	if err != nil {
		s.fail(f.ID, err)
		return
	}
	s.enqueue(encode(typeAck, f.ID, map[string]any{"message": msg}, time.Now()))
}

func (s *Session) onRead(f inFrame) {
	var d struct {
		ChannelID string `json:"channel_id"`
		Seq       *int64 `json:"seq"`
	}
	if json.Unmarshal(f.Data, &d) != nil || d.Seq == nil {
		s.fail(f.ID, httpx.FieldErrors{"seq": "is required"})
		return
	}
	channelID, err := uuid.Parse(d.ChannelID)
	if err != nil {
		s.fail(f.ID, httpx.ErrNotFound("channel not found"))
		return
	}
	if _, err := s.h.msgs.MarkRead(s.ctx, s.tenant.ID, channelID, s.userID, *d.Seq, s.id); err != nil {
		s.fail(f.ID, err)
		return
	}
	if f.ID != "" {
		s.enqueue(encode(typeAck, f.ID, nil, time.Now()))
	}
}

func (s *Session) onTyping(f inFrame, start bool) {
	var d struct {
		ChannelID string `json:"channel_id"`
	}
	channelID, err := uuid.Parse("")
	if json.Unmarshal(f.Data, &d) == nil {
		channelID, err = uuid.Parse(d.ChannelID)
	}
	if err != nil {
		s.fail("", httpx.ErrNotFound("channel not found"))
		return
	}
	now := time.Now()
	s.mu.Lock()
	st := s.typing[channelID]
	if start {
		if st == nil {
			st = &typingState{}
			s.typing[channelID] = st
			st.timer = time.AfterFunc(s.h.opts.TypingExpiry, func() { s.expireTyping(channelID) })
		} else {
			st.timer.Reset(s.h.opts.TypingExpiry)
		}
		if !st.lastStart.IsZero() && now.Sub(st.lastStart) < s.h.opts.TypingMinInterval {
			s.mu.Unlock() // too soon after the last one: the others already know, only the expiry was extended
			return
		}
		st.lastStart = now
	} else {
		if st == nil {
			s.mu.Unlock()
			return
		}
		st.timer.Stop()
		delete(s.typing, channelID)
	}
	s.mu.Unlock()

	if err := s.h.pub.Typing(s.ctx, s.tenant.ID, channelID, s.userID, s.ext, start); err != nil {
		s.mu.Lock()
		if st := s.typing[channelID]; st != nil {
			st.timer.Stop()
			delete(s.typing, channelID)
		}
		s.mu.Unlock()
		if errors.Is(err, ErrNotMember) {
			s.fail("", httpx.ErrNotFound("channel not found"))
			return
		}
		s.h.log.Warn("typing not published", "error", err)
	}
}

// expireTyping ends typing that was not refreshed: a client that crashed mid-sentence must not type forever.
func (s *Session) expireTyping(channelID uuid.UUID) {
	s.mu.Lock()
	_, active := s.typing[channelID]
	delete(s.typing, channelID)
	s.mu.Unlock()
	if !active {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.h.pub.Typing(ctx, s.tenant.ID, channelID, s.userID, s.ext, false)
}

// fail answers a request with an error frame.
func (s *Session) fail(id string, err error) {
	s.enqueue(s.errorFrame(id, err))
}

func (s *Session) errorFrame(id string, err error) []byte {
	var apiErr *httpx.Error
	var fields httpx.FieldErrors
	var data errorData
	switch {
	case errors.As(err, &apiErr):
		data = errorData{Code: apiErr.Code, Message: apiErr.Message, Details: apiErr.Details}
	case errors.As(err, &fields):
		data = errorData{Code: httpx.CodeValidation, Message: fields.Error(), Details: map[string]any{"fields": fields}}
	default:
		s.h.log.Error("websocket request failed", "error", err, "connection_id", s.id)
		data = errorData{Code: httpx.CodeInternal, Message: "internal error"}
	}
	return encode(typeError, id, data, time.Now())
}

// teardown releases everything the session holds, whichever way it ended.
func (s *Session) teardown() {
	if s.tokenTimer != nil {
		s.tokenTimer.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.mu.Lock()
	typingIn := make([]uuid.UUID, 0, len(s.typing))
	for ch, st := range s.typing {
		st.timer.Stop()
		typingIn = append(typingIn, ch)
	}
	s.typing = map[uuid.UUID]*typingState{}
	s.mu.Unlock()
	for _, ch := range typingIn {
		_ = s.h.pub.Typing(ctx, s.tenant.ID, ch, s.userID, s.ext, false)
	}

	if s.registered {
		s.h.hub.Unregister(s)
	}
	if s.online {
		last, seen, err := s.h.presence.Disconnect(ctx, s.tenant.ID, s.userID, s.id)
		if err != nil {
			s.h.log.Warn("presence not cleared", "error", err)
		} else if last {
			s.h.pub.PresenceChanged(ctx, s.tenant.ID, s.userID, s.ext, false, &seen)
		}
	}
	if s.closing.Load() {
		<-s.closeDone
	} else {
		_ = s.conn.CloseNow()
	}
	if m := s.h.metrics; m != nil {
		m.WSClosed.WithLabelValues(fmt.Sprint(s.closeCode.Load())).Inc()
	}
}
