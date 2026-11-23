package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/rizkysiregar/marpokat/server/internal/console"
	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

const accountUsage = `usage: marpokat account <command> [flags]

commands:
  promote  --email E   make the account a platform administrator (there is no API for this on purpose)
  demote   --email E   take it away

In the cluster: kubectl -n marpokat-dev exec deploy/marpokat-api -- marpokat account promote --email you@example.com
`

type accountCmdConfig struct {
	Postgres config.Postgres
}

// RunAccount runs `marpokat account ...`.
func RunAccount(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(stdout, accountUsage)
		return nil
	}
	fs := flag.NewFlagSet("account "+args[0], flag.ContinueOnError)
	email := fs.String("email", "", "e-mail of the account")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("--email is required")
	}
	if args[0] != "promote" && args[0] != "demote" {
		return fmt.Errorf("unknown account command %q\n\n%s", args[0], accountUsage)
	}
	cfg, err := config.Load[accountCmdConfig]()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := store.New(pool)
	svc := console.NewService(q, tenant.NewService(q, nil), console.LogMailer{Log: slog.Default()}, nil, slog.Default(), console.Options{})
	if err := svc.PromoteAdmin(ctx, *email, args[0] == "promote"); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: %s\n", args[0], *email)
	return nil
}
