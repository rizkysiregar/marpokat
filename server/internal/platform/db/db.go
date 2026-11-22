// Package db opens PostgreSQL connections.
//
// In every environment the database is reached through pgbouncer in transaction mode: two statements outside a
// transaction may run on different server connections. Named prepared statements (pgx's default) would then fail
// with "prepared statement does not exist", so every connection uses the simple protocol. LISTEN/NOTIFY does not
// work through pgbouncer either, which is why the outbox relay polls (PLAN D7).
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
)

// Open returns a connection pool and checks that the database answers.
func Open(ctx context.Context, c config.Postgres) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(c.DSN())
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pc.MaxConns = c.MaxConns
	pc.MinConns = 1
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.MaxConnLifetime = 30 * time.Minute
	pc.HealthCheckPeriod = 30 * time.Second
	pc.ConnConfig.ConnectTimeout = 10 * time.Second
	pc.ConnConfig.RuntimeParams["application_name"] = "marpokat"

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping %s:%d/%s: %w", c.Host, c.Port, c.Database, err)
	}
	return pool, nil
}

// OpenSQL returns a database/sql handle with the same protocol settings, for tools that need one (goose).
func OpenSQL(c config.Postgres) (*sql.DB, error) {
	cc, err := pgx.ParseConfig(c.DSN())
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cc.RuntimeParams["application_name"] = "marpokat-migrate"
	return stdlib.OpenDB(*cc), nil
}
