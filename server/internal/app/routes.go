package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

// Deps is what the public router needs. Integration tests build the same router with a test database.
type Deps struct {
	Info Info
	Pool *pgxpool.Pool
	Box  *secretbox.Box
	Log  *slog.Logger
}

// PublicRoutes builds the public router: every feature module registers its routes here.
func PublicRoutes(d Deps) http.Handler {
	tenants := tenant.NewService(store.New(d.Pool), d.Box)
	users := user.NewService(d.Pool)
	mw := auth.NewMiddleware(tenant.NewResolver(tenants.Credentials), userLookup(users), d.Log)

	mux := http.NewServeMux()
	// reachable through Cloudflare, used to check the tunnel and ingress; no dependency checks
	mux.HandleFunc("GET /healthz", health.LiveHandler)
	mux.HandleFunc("GET /v1", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"name": "marpokat", "version": d.Info.Version})
	})
	user.NewHandler(users, d.Log).Register(mux, mw.User, mw.Server)
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
