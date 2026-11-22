package app

import (
	"context"
	"fmt"
	"os"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/platform/migrate"
)

type migrateConfig struct {
	Server   config.Server
	Postgres config.Postgres
}

// RunMigrate runs `migrate up|down|status`. Only Postgres settings are required.
func RunMigrate(ctx context.Context, info Info, action string) error {
	cfg, err := config.Load[migrateConfig]()
	if err != nil {
		return err
	}
	log := NewLogger(cfg.Server.LogLevel, "migrate", info)

	sqlDB, err := db.OpenSQL(cfg.Postgres)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("migrate: ping %s:%d/%s: %w", cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Database, err)
	}

	switch action {
	case "up":
		return migrate.Up(ctx, sqlDB, log)
	case "down":
		return migrate.Down(ctx, sqlDB, log)
	case "status":
		return migrate.Status(ctx, sqlDB, os.Stdout)
	default:
		return fmt.Errorf("unknown migrate action %q (want up, down or status)", action)
	}
}
