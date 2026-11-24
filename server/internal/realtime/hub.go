package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

var (
	errHubClosed   = errors.New("realtime: shutting down")
	errHubNotReady = errors.New("realtime: not connected to Redis")
)

// Hub holds the WebSocket sessions of one pod and the Redis subscriptions that feed them: one topic per connected
// user, subscribed while that user has a session here. It is the only reader of those topics.
//
// A Redis Pub/Sub message that nobody receives is gone, so the Hub watches its own subscription. When Redis dropped
// the connection and the client library silently rebuilt it, or when it stops answering, the Hub closes every session
// with 1012: clients reconnect and sync, which is exactly how they repair any other gap.
type Hub struct {
	id        string
	rdb       *redisx.Client
	sub       *redis.Client
	log       *slog.Logger
	opts      Options
	beatTopic string
	ctlTopic  string
	metrics   *metrics.Registry // may be nil

	mu              sync.Mutex
	topics          map[string]map[*Session]struct{}
	tenantListeners []func(uuid.UUID)
	ps              *redis.PubSub
	closed          bool

	connects    atomic.Int64 // Redis connections the subscriber opened since the subscription started
	reconnected chan struct{}
	lastBeat    atomic.Int64 // unix nanoseconds of the last beat that came back through Redis
}

func NewHub(rdb *redisx.Client, log *slog.Logger, opts Options, m *metrics.Registry) *Hub {
	id := uuid.NewString()
	h := &Hub{
		id:          id,
		rdb:         rdb,
		log:         log.With("subsystem", "realtime-hub", "hub_id", id),
		opts:        opts,
		beatTopic:   rdb.Key("hub", id),
		ctlTopic:    controlTopic(rdb),
		metrics:     m,
		topics:      map[string]map[*Session]struct{}{},
		reconnected: make(chan struct{}, 1),
	}
	h.sub = rdb.Subscriber(h.onConnect)
	return h
}

func (h *Hub) onConnect() {
	if h.connects.Add(1) > 1 {
		select {
		case h.reconnected <- struct{}{}:
		default:
		}
	}
}

// Run keeps the subscription alive until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	defer func() { _ = h.sub.Close() }()
	for ctx.Err() == nil {
		err := h.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("redis subscription lost, closing sessions so clients resync", "error", err)
		h.closeAll(CloseServerRestart, "realtime restarting")
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// serve subscribes and dispatches until the subscription breaks (returns the reason) or ctx ends.
func (h *Hub) serve(ctx context.Context) error {
	h.connects.Store(0)
	select {
	case <-h.reconnected:
	default:
	}
	ps := h.sub.Subscribe(ctx, h.beatTopic, h.ctlTopic)
	confirm, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := ps.Receive(confirm)
	cancel()
	if err != nil {
		_ = ps.Close()
		return fmt.Errorf("subscribe: %w", err)
	}
	h.mu.Lock()
	topics := make([]string, 0, len(h.topics))
	for t := range h.topics { // sessions that are still closing from the last round: harmless, they unsubscribe when gone
		topics = append(topics, t)
	}
	h.ps = ps
	h.mu.Unlock()
	if len(topics) > 0 {
		if err := ps.Subscribe(ctx, topics...); err != nil {
			_ = ps.Close()
			return fmt.Errorf("resubscribe: %w", err)
		}
	}
	defer func() {
		h.mu.Lock()
		if h.ps == ps {
			h.ps = nil
		}
		h.mu.Unlock()
		_ = ps.Close()
	}()

	h.lastBeat.Store(time.Now().UnixNano())
	h.log.Info("redis subscription ready")
	ch := ps.Channel(redis.WithChannelSize(4096), redis.WithChannelHealthCheckInterval(5*time.Second))
	ticker := time.NewTicker(h.opts.HubBeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return errors.New("subscription channel closed")
			}
			h.dispatch(m)
		case <-h.reconnected:
			// The library reconnected by itself and resubscribed: whatever was published meanwhile is lost.
			h.log.Warn("redis connection was re-established, closing sessions so clients resync")
			h.closeAll(CloseServerRestart, "realtime restarting")
		case <-ticker.C:
			beatCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = h.rdb.Publish(beatCtx, h.beatTopic, "beat").Err()
			cancel()
			if since := time.Since(time.Unix(0, h.lastBeat.Load())); since > h.opts.HubBeatTimeout {
				return fmt.Errorf("no beat for %s", since.Round(time.Second))
			}
		}
	}
}

// dispatch writes an event to the sessions subscribed to its topic.
func (h *Hub) dispatch(m *redis.Message) {
	if m.Channel == h.beatTopic {
		h.lastBeat.Store(time.Now().UnixNano())
		return
	}
	if m.Channel == h.ctlTopic {
		h.control(m.Payload)
		return
	}
	var w wire
	if err := json.Unmarshal([]byte(m.Payload), &w); err != nil || w.Type == "" {
		h.log.Warn("dropping a malformed event", "topic", m.Channel, "error", err)
		return
	}
	h.mu.Lock()
	sessions := make([]*Session, 0, len(h.topics[m.Channel]))
	for s := range h.topics[m.Channel] {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	if len(sessions) == 0 {
		return
	}
	frame := encode(w.Type, "", w.Data, time.Now())
	for _, s := range sessions {
		if w.Origin != "" && w.Origin == s.id {
			continue // that connection caused the event and was already answered
		}
		s.enqueue(frame)
		if h.metrics != nil {
			h.metrics.WSEventsDelivered.WithLabelValues(w.Type).Inc()
		}
	}
}

// Register lets the session receive events for its user.
func (h *Hub) Register(ctx context.Context, s *Session) error {
	topic := userTopic(h.rdb, s.tenant.ID, s.userID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errHubClosed
	}
	set := h.topics[topic]
	if set == nil {
		if h.ps == nil {
			return errHubNotReady
		}
		subCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := h.ps.Subscribe(subCtx, topic); err != nil {
			return fmt.Errorf("subscribe %s: %w", topic, err)
		}
		set = map[*Session]struct{}{}
		h.topics[topic] = set
	}
	set[s] = struct{}{}
	return nil
}

// Unregister stops events for the session, and the subscription when it was the user's last session here.
func (h *Hub) Unregister(s *Session) {
	topic := userTopic(h.rdb, s.tenant.ID, s.userID)
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.topics[topic]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) > 0 {
		return
	}
	delete(h.topics, topic)
	if h.ps != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.ps.Unsubscribe(ctx, topic); err != nil {
			h.log.Warn("cannot unsubscribe", "topic", topic, "error", err)
		}
	}
}

// Ready reports whether the Redis subscription is up, so sessions can register. A client that connects before it
// is told to retry (1011), which a starting pod only ever does for a moment.
func (h *Hub) Ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ps != nil
}

// Sessions is how many sessions this pod holds.
func (h *Hub) Sessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.topics {
		n += len(set)
	}
	return n
}

// Shutdown closes every session with 1012 and refuses new ones. Called when the HTTP server shuts down, after the pod
// stopped receiving traffic, so the clients reconnect to another pod.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.closeAll(CloseServerRestart, "server restarting")
}

func (h *Hub) closeAll(code websocket.StatusCode, reason string) {
	h.mu.Lock()
	var all []*Session
	for _, set := range h.topics {
		for s := range set {
			all = append(all, s)
		}
	}
	h.mu.Unlock()
	for _, s := range all {
		s.requestClose(code, reason)
	}
}
