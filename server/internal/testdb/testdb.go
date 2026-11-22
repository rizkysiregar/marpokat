//go:build integration

// Package testdb starts one PostgreSQL container per test binary and applies the migrations.
//
// Integration tests are behind the `integration` build tag (`make test-integration`) because they need Docker.
// Tests share the database and stay independent by creating their own tenants: every row is tenant scoped.
package testdb

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
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
	pool *pgxpool.Pool

	// Redis and RabbitMQ start the first time a test asks for them, so packages that need only Postgres stay fast.
	redisOnce  sync.Once
	redisConf  config.Redis
	redisCtr   testcontainers.Container
	redisErr   error
	rabbitOnce sync.Once
	rabbitConf config.RabbitMQ
	rabbitCtr  testcontainers.Container
	rabbitErr  error
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

	defer func() {
		for _, c := range []testcontainers.Container{redisCtr, rabbitCtr} {
			if c != nil {
				_ = testcontainers.TerminateContainer(c)
			}
		}
	}()
	return m.Run()
}

// Redis returns the settings of the shared Redis container, starting it on first use. Tests stay apart through the
// ids in their keys.
func Redis(t *testing.T) config.Redis {
	t.Helper()
	redisOnce.Do(func() {
		ctx := context.Background()
		redisCtr, redisErr = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "redis:7-alpine",
				ExposedPorts: []string{"6379/tcp"},
				WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
			},
			Started: true,
		})
		if redisErr != nil {
			return
		}
		host, err := redisCtr.Host(ctx)
		if err != nil {
			redisErr = err
			return
		}
		port, err := redisCtr.MappedPort(ctx, "6379/tcp")
		if err != nil {
			redisErr = err
			return
		}
		redisConf = config.Redis{Host: host, Port: int(port.Num()), KeyPrefix: "marpokat-test:"}
	})
	if redisErr != nil {
		t.Fatalf("testdb: redis container: %v", redisErr)
	}
	return redisConf
}

// RabbitMQ returns the settings of the shared RabbitMQ container (default vhost, user marpokat), starting it on
// first use. Tests stay apart through the name prefix of their exchanges and queues.
func RabbitMQ(t *testing.T) config.RabbitMQ {
	t.Helper()
	rabbitOnce.Do(func() {
		ctx := context.Background()
		rabbitCtr, rabbitErr = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "rabbitmq:4-alpine",
				ExposedPorts: []string{"5672/tcp"},
				// the default guest user may only connect from localhost, which a mapped port is not
				Env:        map[string]string{"RABBITMQ_DEFAULT_USER": "marpokat", "RABBITMQ_DEFAULT_PASS": "marpokat"},
				WaitingFor: wait.ForLog("Server startup complete").WithStartupTimeout(120 * time.Second),
			},
			Started: true,
		})
		if rabbitErr != nil {
			return
		}
		host, err := rabbitCtr.Host(ctx)
		if err != nil {
			rabbitErr = err
			return
		}
		port, err := rabbitCtr.MappedPort(ctx, "5672/tcp")
		if err != nil {
			rabbitErr = err
			return
		}
		rabbitConf = config.RabbitMQ{Host: host, Port: int(port.Num()), Username: "marpokat", Password: "marpokat", VirtualHost: "/"}
	})
	if rabbitErr != nil {
		t.Fatalf("testdb: rabbitmq container: %v", rabbitErr)
	}
	return rabbitConf
}

// RabbitCtl runs rabbitmqctl inside the container, to take the broker away and bring it back (stop_app, start_app)
// without changing its address.
func RabbitCtl(t *testing.T, args ...string) {
	t.Helper()
	RabbitMQ(t)
	code, out, err := rabbitCtr.Exec(context.Background(), append([]string{"rabbitmqctl"}, args...))
	if err != nil {
		t.Fatalf("rabbitmqctl %v: %v", args, err)
	}
	if code != 0 {
		body, _ := io.ReadAll(out)
		t.Fatalf("rabbitmqctl %v exited with %d: %s", args, code, fmt.Sprint(string(body)))
	}
}

// Pool returns the shared pool. Only valid inside tests run through Main.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if pool == nil {
		t.Fatal("testdb: Main was not called from TestMain")
	}
	return pool
}
