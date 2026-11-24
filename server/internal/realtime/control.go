package realtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

// Control messages go to every pod on one Redis topic. The only one so far: a tenant changed (switched off, deleted,
// secret rotated, settings changed). Every pod then forgets what it cached about that tenant, and when it was switched
// off closes its WebSockets at once with 4003: the clients' credentials are no longer accepted, so they stop
// reconnecting (spec/events.md). Before this, an open socket outlived a disabled tenant until its token expired.

const controlTenantChanged = "tenant.changed"

type controlMessage struct {
	Type     string    `json:"type"`
	TenantID uuid.UUID `json:"tenant_id"`
	Disabled bool      `json:"disabled"`
}

func controlTopic(rdb *redisx.Client) string { return rdb.Key("ctl") }

// TenantNotifier tells every pod that a tenant changed. It implements tenant.Notifier.
type TenantNotifier struct{ rdb *redisx.Client }

func NewTenantNotifier(rdb *redisx.Client) *TenantNotifier { return &TenantNotifier{rdb: rdb} }

func (n *TenantNotifier) TenantChanged(ctx context.Context, tenantID uuid.UUID, disabled bool) error {
	b, err := json.Marshal(controlMessage{Type: controlTenantChanged, TenantID: tenantID, Disabled: disabled})
	if err != nil {
		return err
	}
	if err := n.rdb.Publish(ctx, controlTopic(n.rdb), b).Err(); err != nil {
		return fmt.Errorf("realtime: announce tenant change: %w", err)
	}
	return nil
}

// OnTenantChanged registers fn, called on this pod for every tenant change announced by any pod (the API key cache
// forgets the tenant). Safe to call while the hub runs.
func (h *Hub) OnTenantChanged(fn func(tenantID uuid.UUID)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tenantListeners = append(h.tenantListeners, fn)
}

func (h *Hub) control(payload string) {
	var m controlMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m.Type != controlTenantChanged || m.TenantID == uuid.Nil {
		h.log.Warn("dropping a malformed control message", "error", err)
		return
	}
	h.mu.Lock()
	listeners := append([]func(uuid.UUID){}, h.tenantListeners...)
	var doomed []*Session
	if m.Disabled {
		for _, set := range h.topics {
			for s := range set {
				if s.tenant.ID == m.TenantID {
					doomed = append(doomed, s)
				}
			}
		}
	}
	h.mu.Unlock()
	for _, fn := range listeners {
		fn(m.TenantID)
	}
	for _, s := range doomed {
		s.requestClose(CloseAuthFailed, "this app is switched off")
	}
	if len(doomed) > 0 {
		h.log.Info("closed the sessions of a switched off tenant", "tenant", m.TenantID, "sessions", len(doomed))
	}
}
