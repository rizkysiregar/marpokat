package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
)

func TestDrainThenStop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := health.NewRegistry()
	parent, signal := context.WithCancel(context.Background())

	stopCtx, cancel := drainThenStop(parent, reg, 50*time.Millisecond, log)
	defer cancel()

	if _, ok := reg.Run(context.Background()); !ok {
		t.Fatal("ready before shutdown was requested")
	}

	start := time.Now()
	signal()
	time.Sleep(10 * time.Millisecond)
	if _, ok := reg.Run(context.Background()); ok {
		t.Fatal("still ready after shutdown was requested")
	}
	if stopCtx.Err() != nil {
		t.Fatal("servers stopped before the drain delay")
	}

	<-stopCtx.Done()
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("stopped after %v, before the 50ms delay", elapsed)
	}
}
