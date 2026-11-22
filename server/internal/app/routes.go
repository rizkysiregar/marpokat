package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/channel"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/ratelimit"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

// Deps is what the public router needs. Integration tests build the same router with a test database.
// Without Redis there is no rate limiting and no realtime; with Redis but without a Hub there is no WebSocket.
type Deps struct {
	Info    Info
	Pool    *pgxpool.Pool
	Box     *secretbox.Box
	Log     *slog.Logger
	Redis   *redisx.Client
	Hub     *realtime.Hub
	Metrics *metrics.Registry
	// Realtime overrides the realtime timings; the zero value means realtime.DefaultOptions().
	Realtime realtime.Options
}

// PublicRoutes builds the public router: every feature module registers its routes here.
func PublicRoutes(d Deps) http.Handler {
	tenants := tenant.NewService(store.New(d.Pool), d.Box)
	users := user.NewService(d.Pool)
	resolver := tenant.NewResolver(tenants.Credentials)
	lookup := userLookup(users)
	mw := auth.NewMiddleware(resolver, lookup, d.Log)
	channels := channel.NewService(d.Pool)
	messages := message.NewService(d.Pool)

	mux := http.NewServeMux()
	// reachable through Cloudflare, used to check the tunnel and ingress; no dependency checks
	mux.HandleFunc("GET /healthz", health.LiveHandler)
	mux.HandleFunc("GET /v1", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"name": "marpokat", "version": d.Info.Version})
	})
	user.NewHandler(users, d.Log).Register(mux, mw.User, mw.Server)
	if d.Redis != nil {
		messages.SetLimiter(ratelimit.New(d.Redis, d.Log))
	}
	if d.Redis != nil && d.Hub != nil {
		opts := d.Realtime
		if opts == (realtime.Options{}) {
			opts = realtime.DefaultOptions()
		}
		publisher := realtime.NewPublisher(d.Redis, store.New(d.Pool), channels, d.Log)
		channels.SetNotifier(publisher)
		messages.SetNotifier(publisher)
		realtime.NewHandler(realtime.Deps{
			Hub: d.Hub, Publisher: publisher, Presence: realtime.NewPresence(d.Redis, opts.PresenceTTL),
			Tenants: resolver, Users: lookup, UserSvc: users, Messages: messages, Queries: store.New(d.Pool),
			Options: opts, Log: d.Log, Metrics: d.Metrics,
		}).Register(mux, mw.User)
	}
	channel.NewHandler(channels, d.Log).Register(mux, mw.User, mw.Server)
	message.NewHandler(messages, d.Log).Register(mux, mw.User, mw.Server)
	mux.HandleFunc("/", httpx.NotFound)
	return mux
}

func userLookup(users *user.Service) auth.UserLookup {
	return func(ctx context.Context, tenantID uuid.UUID, externalID string) (uuid.UUID, error) {
		id, err := users.ResolveID(ctx, tenantID, externalID)
		if errors.Is(err, user.ErrNotFound) {
			return uuid.Nil, auth.ErrUnknownUser
		}
		return id, err
	}
}
