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

var (
	pool      *pgxpool.Pool
	redisConf config.Redis
)

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

	rctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		slog.Error("start redis container", "error", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(rctr) }()
	rhost, err := rctr.Host(ctx)
	if err != nil {
		slog.Error("redis host", "error", err)
		return 1
	}
	rport, err := rctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		slog.Error("redis port", "error", err)
		return 1
	}
	redisConf = config.Redis{Host: rhost, Port: int(rport.Num()), KeyPrefix: "marpokat-test:"}
	return m.Run()
}

// Redis returns the settings of the shared Redis container. Tests stay apart through the ids in their keys.
func Redis(t *testing.T) config.Redis {
	t.Helper()
	if redisConf.Host == "" {
		t.Fatal("testdb: Main was not called from TestMain")
	}
	return redisConf
}

// Pool returns the shared pool. Only valid inside tests run through Main.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if pool == nil {
		t.Fatal("testdb: Main was not called from TestMain")
	}
	return pool
}
