//go:build integration

// Package testdb starts one PostgreSQL container per test binary and applies the migrations.
//
// Integration tests are behind the `integration` build tag (`make test-integration`) because they need Docker.
// Tests share the database and stay independent by creating their own tenants: every row is tenant scoped.
package testdb

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/platform/migrate"
)

var pool *pgxpool.Pool

// Main is called from TestMain: it starts the container, migrates, runs the tests and cleans up.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("marpokat"),
		tcpostgres.WithUsername("marpokat"),
		tcpostgres.WithPassword("marpokat"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		slog.Error("start postgres container", "error", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	host, err := ctr.Host(ctx)
	if err != nil {
		slog.Error("container host", "error", err)
		return 1
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		slog.Error("container port", "error", err)
		return 1
	}
	cfg := config.Postgres{Host: host, Port: int(port.Num()), Database: "marpokat", User: "marpokat", Password: "marpokat", SSLMode: "disable", MaxConns: 10}

	sqlDB, err := db.OpenSQL(cfg)
	if err != nil {
		slog.Error("open sql", "error", err)
		return 1
	}
	if err := migrate.Up(ctx, sqlDB, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		slog.Error("migrate", "error", err)
		return 1
	}
	_ = sqlDB.Close()

	pool, err = db.Open(ctx, cfg)
	if err != nil {
		slog.Error("open pool", "error", err)
		return 1
	}
	defer pool.Close()
	return m.Run()
}

// Pool returns the shared pool. Only valid inside tests run through Main.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if pool == nil {
		t.Fatal("testdb: Main was not called from TestMain")
	}
	return pool
}
