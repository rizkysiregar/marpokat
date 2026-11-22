package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

const tenantUsage = `usage: marpokat tenant <command> [flags]

commands:
  create         --slug S [--name N] [--client-can-create-dm] [--client-can-create-group]
  list
  show           --slug S
  rotate-secret  --slug S
  set-config     --slug S [--client-can-create-dm=true|false] [--client-can-create-group=true|false]
  set-fcm        --slug S --file PATH   (Firebase service account JSON, "-" reads stdin)
  set-webhook    --slug S --url URL     (empty --url removes the webhook)
  disable        --slug S
  enable         --slug S
  token          --slug S --user EXTERNAL_ID [--ttl 1h]   sign a user token (testing)
  token          --slug S --server                         sign a server token valid 5 minutes (testing)

In the cluster (the image has no shell, the binary is called directly):
  kubectl -n marpokat-dev exec deploy/marpokat-api -- marpokat tenant list
`

type tenantCmdConfig struct {
	Postgres config.Postgres
	Secrets  config.Secrets
}

// RunTenant runs `marpokat tenant ...`. Secrets are printed to stdout once; nothing is logged.
func RunTenant(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(stdout, tenantUsage)
		return nil
	}
	cfg, err := config.Load[tenantCmdConfig]()
	if err != nil {
		return err
	}
	box, err := openBox(cfg.Secrets)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runTenantCommand(ctx, tenant.NewService(store.New(pool), box), args, stdin, stdout)
}

func runTenantCommand(ctx context.Context, svc *tenant.Service, args []string, stdin io.Reader, stdout io.Writer) error {
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("tenant "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	slug := fs.String("slug", "", "tenant slug")
	name := fs.String("name", "", "display name")
	dm := fs.Bool("client-can-create-dm", false, "clients may create DMs themselves")
	group := fs.Bool("client-can-create-group", false, "clients may create groups themselves")
	file := fs.String("file", "", "service account JSON path or -")
	rawURL := fs.String("url", "", "webhook URL")
	userID := fs.String("user", "", "external user id")
	server := fs.Bool("server", false, "sign a server token")
	ttl := fs.Duration("ttl", time.Hour, "user token lifetime")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%w\n\n%s", err, tenantUsage)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	needSlug := func() error {
		if *slug == "" {
			return errors.New("--slug is required")
		}
		return nil
	}

	switch cmd {
	case "create":
		if err := needSlug(); err != nil {
			return err
		}
		t, secret, err := svc.Create(ctx, *slug, *name, tenant.Config{ClientCanCreateDM: *dm, ClientCanCreateGroup: *group})
		if err != nil {
			return err
		}
		printTenant(stdout, t)
		_, _ = fmt.Fprintf(stdout, "api_secret:  %s\n\nThe api secret is shown only now. Store it in the tenant backend's secrets; never ship it in an app.\n", secret)
		return nil

	case "list":
		ts, err := svc.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SLUG\tNAME\tAPI KEY\tDM\tGROUP\tFCM\tSTATUS\tCREATED")
		for _, t := range ts {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%v\t%v\t%s\t%s\n", t.Slug, t.Name, t.APIKey,
				t.Config.ClientCanCreateDM, t.Config.ClientCanCreateGroup, t.HasFCM, status(t), t.CreatedAt.UTC().Format(time.DateOnly))
		}
		return tw.Flush()

	case "show":
		if err := needSlug(); err != nil {
			return err
		}
		t, err := svc.GetBySlug(ctx, *slug)
		if err != nil {
			return err
		}
		printTenant(stdout, t)
		return nil

	case "rotate-secret":
		if err := needSlug(); err != nil {
			return err
		}
		secret, err := svc.RotateSecret(ctx, *slug)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "api_secret:  %s\n\nThe old secret stops working within 30 seconds. Update the tenant backend now.\n", secret)
		return nil

	case "set-config":
		if err := needSlug(); err != nil {
			return err
		}
		t, err := svc.GetBySlug(ctx, *slug)
		if err != nil {
			return err
		}
		cfg := t.Config
		if set["client-can-create-dm"] {
			cfg.ClientCanCreateDM = *dm
		}
		if set["client-can-create-group"] {
			cfg.ClientCanCreateGroup = *group
		}
		if err := svc.SetConfig(ctx, *slug, cfg); err != nil {
			return err
		}
		t.Config = cfg
		printTenant(stdout, t)
		return nil

	case "set-fcm":
		if err := needSlug(); err != nil {
			return err
		}
		var data []byte
		var err error
		switch *file {
		case "":
			return errors.New("--file is required (path, or - for stdin)")
		case "-":
			data, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
		default:
			data, err = os.ReadFile(*file)
		}
		if err != nil {
			return fmt.Errorf("read FCM credentials: %w", err)
		}
		if err := svc.SetFCMCredentials(ctx, *slug, data); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, "FCM credentials stored (encrypted).")
		return nil

	case "set-webhook":
		if err := needSlug(); err != nil {
			return err
		}
		secret, err := svc.SetWebhook(ctx, *slug, *rawURL)
		if err != nil {
			return err
		}
		if secret == "" {
			_, _ = fmt.Fprintln(stdout, "Webhook removed.")
			return nil
		}
		_, _ = fmt.Fprintf(stdout, "webhook_secret:  %s\n\nVerify X-Marpokat-Signature with it. Shown only now.\n", secret)
		return nil

	case "disable", "enable":
		if err := needSlug(); err != nil {
			return err
		}
		if err := svc.SetDisabled(ctx, *slug, cmd == "disable"); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Tenant %s %sd (running pods notice within 30 seconds).\n", *slug, cmd)
		return nil

	case "token":
		if err := needSlug(); err != nil {
			return err
		}
		t, err := svc.GetBySlug(ctx, *slug)
		if err != nil {
			return err
		}
		secret, err := svc.Secret(t)
		if err != nil {
			return err
		}
		var tok string
		switch {
		case *server:
			tok, err = auth.SignServerToken(secret, auth.MaxServerTokenLifetime, time.Now())
		case *userID != "":
			tok, err = auth.SignUserToken(secret, *userID, *ttl, time.Now())
		default:
			return errors.New("--user EXTERNAL_ID or --server is required")
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, tok)
		return nil

	default:
		return fmt.Errorf("unknown tenant command %q\n\n%s", cmd, tenantUsage)
	}
}

func printTenant(w io.Writer, t tenant.Tenant) {
	webhook := "-"
	if t.WebhookURL != nil {
		webhook = *t.WebhookURL
	}
	_, _ = fmt.Fprintf(w, strings.TrimLeft(`
slug:        %s
name:        %s
id:          %s
api_key:     %s
config:      client_can_create_dm=%v client_can_create_group=%v
fcm:         %v
webhook:     %s
status:      %s
`, "\n"), t.Slug, t.Name, t.ID, t.APIKey, t.Config.ClientCanCreateDM, t.Config.ClientCanCreateGroup, t.HasFCM, webhook, status(t))
}

func status(t tenant.Tenant) string {
	if t.Disabled() {
		return "disabled"
	}
	return "active"
}
