// Command marpokat is the single binary of the Marpokat chat platform (PLAN D2).
//
//	marpokat api               public REST + WebSocket API
//	marpokat worker            background jobs (outbox relay, push, webhook, media)
//	marpokat migrate [action]  database migrations: up (default), down, status
//	marpokat tenant <command>  manage tenants (create, list, rotate-secret, token, ...)
//	marpokat version
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rizkysiregar/marpokat/server/internal/app"
)

// Set at build time: -ldflags "-X main.version=... -X main.commit=..."
var (
	version = "dev"
	commit  = "unknown"
)

const usage = `usage: marpokat <command>

commands:
  api                 serve the public REST + WebSocket API
  worker              run background jobs
  migrate [up|down|status]
                      apply database migrations (default: up)
  tenant <command>    manage tenants, see: marpokat tenant help
  version             print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	info := app.Info{Version: version, Commit: commit}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd := os.Args[1]; cmd {
	case "api":
		err = app.RunAPI(ctx, info)
	case "worker":
		err = app.RunWorker(ctx, info)
	case "migrate":
		action := "up"
		if len(os.Args) > 2 {
			action = os.Args[2]
		}
		err = app.RunMigrate(ctx, info, action)
	case "tenant":
		err = app.RunTenant(ctx, os.Args[2:], os.Stdin, os.Stdout)
	case "version":
		fmt.Printf("marpokat %s (%s)\n", version, commit)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		// one JSON line, so it is readable in the same log pipeline as everything else
		fmt.Fprintf(os.Stderr, "{\"level\":\"ERROR\",\"service\":\"marpokat\",\"msg\":%q}\n", err.Error())
		os.Exit(1)
	}
}
