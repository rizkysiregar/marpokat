package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestPostgresDSNEscapesPassword(t *testing.T) {
	p := Postgres{Host: "db", Port: 6432, Database: "marpokat_dev", User: "marpokat", Password: "p@ss/w:rd", SSLMode: "disable"}
	got := p.DSN()
	want := "postgres://marpokat:p%40ss%2Fw%3Ard@db:6432/marpokat_dev?sslmode=disable"
	if got != want {
		t.Fatalf("DSN() = %q, want %q", got, want)
	}
}

func TestRabbitURLEscapesVhost(t *testing.T) {
	r := RabbitMQ{Host: "mq", Port: 5672, Username: "u", Password: "p", VirtualHost: "/marpokat-dev"}
	if got, want := r.URL(), "amqp://u:p@mq:5672/%2Fmarpokat-dev"; got != want {
		t.Fatalf("URL() = %q, want %q", got, want)
	}
}

func TestMinIOHostAndTLS(t *testing.T) {
	cases := []struct {
		endpoint string
		host     string
		tls      bool
		wantErr  bool
	}{
		{"https://infra-minio.rizkysiregar.my.id/", "infra-minio.rizkysiregar.my.id", true, false},
		{"http://localhost:9000", "localhost:9000", false, false},
		{"localhost:9000", "", false, true},
		{"ftp://x", "", false, true},
	}
	for _, c := range cases {
		host, tls, err := MinIO{Endpoint: c.endpoint}.HostAndTLS()
		if (err != nil) != c.wantErr {
			t.Fatalf("%q: err = %v, wantErr %v", c.endpoint, err, c.wantErr)
		}
		if host != c.host || tls != c.tls {
			t.Fatalf("%q: got (%q, %v), want (%q, %v)", c.endpoint, host, tls, c.host, c.tls)
		}
	}
}

func TestMasterKeyBytes(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := (Secrets{MasterKey: good}).MasterKeyBytes(); err != nil {
		t.Fatalf("32 byte key rejected: %v", err)
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := (Secrets{MasterKey: short}).MasterKeyBytes(); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("16 byte key: err = %v, want length error", err)
	}
	if _, err := (Secrets{MasterKey: "not base64!"}).MasterKeyBytes(); err == nil {
		t.Fatal("invalid base64 accepted")
	}
}

func TestLoadRequiresVariables(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "")
	if _, err := Load[Postgres](); err == nil {
		t.Fatal("Load[Postgres] succeeded without required variables")
	}

	t.Setenv("POSTGRES_HOST", "db")
	t.Setenv("POSTGRES_DATABASE", "marpokat_dev")
	t.Setenv("POSTGRES_USER", "marpokat")
	t.Setenv("POSTGRES_PASSWORD", "secret")
	p, err := Load[Postgres]()
	if err != nil {
		t.Fatalf("Load[Postgres]: %v", err)
	}
	if p.Port != 5432 || p.MaxConns != 10 || p.SSLMode != "disable" {
		t.Fatalf("defaults not applied: %+v", p)
	}
}

func TestConsoleValidate(t *testing.T) {
	proxies := []string{"10.0.0.0/8", "127.0.0.0/8"}
	good := Console{Addr: ":8081", BaseURL: "https://marpokat.example.com", MailDriver: "log", TrustedProxies: proxies, DeletionGrace: 72 * time.Hour,
		AllowedOrigins: []string{"http://localhost:3000"}, AdminOrigins: []string{"https://admin.marpokat.example.com"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good config was refused: %v", err)
	}
	if err := (Console{}).Validate(); err != nil {
		t.Fatalf("a disabled console must not need anything: %v", err)
	}
	for name, mutate := range map[string]func(*Console){
		"no base url":              func(c *Console) { c.BaseURL = "" },
		"bad base url":             func(c *Console) { c.BaseURL = "marpokat.example.com" },
		"origin with a path":       func(c *Console) { c.AllowedOrigins = []string{"https://a.example.com/app"} },
		"origin without a scheme":  func(c *Console) { c.AllowedOrigins = []string{"a.example.com"} },
		"admin origin not a URL":   func(c *Console) { c.AdminOrigins = []string{"ftp://admin.example.com"} },
		"admin on the portal host": func(c *Console) { c.AdminOrigins = []string{"https://MARPOKAT.example.com"} },
		"bad proxy":                func(c *Console) { c.TrustedProxies = []string{"10.0.0.0"} },
		"grace too short":          func(c *Console) { c.DeletionGrace = time.Minute },
		"bad turnstile url":        func(c *Console) { c.TurnstileSecretKey = "s"; c.TurnstileVerifyURL = "nope" },
		"unknown driver":           func(c *Console) { c.MailDriver = "pigeon" },
		"smtp without host":        func(c *Console) { c.MailDriver = "smtp"; c.MailFrom = "a@b.co" },
		"smtp without from":        func(c *Console) { c.MailDriver = "smtp"; c.MailHost = "smtp.example.com" },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConsoleOriginsAndProxies(t *testing.T) {
	c := Console{BaseURL: "https://Marpokat.example.com/some/page", AllowedOrigins: []string{" http://localhost:3000/ "},
		AdminOrigins: []string{"https://Admin.example.com"}, TrustedProxies: []string{"10.1.2.3/8", " ", "::1/128"}}
	portal, err := c.PortalOrigins()
	if err != nil || len(portal) != 2 || portal[0] != "https://marpokat.example.com" || portal[1] != "http://localhost:3000" {
		t.Fatalf("portal origins: %v %v", portal, err)
	}
	if admin, err := c.AdminOriginList(); err != nil || len(admin) != 1 || admin[0] != "https://admin.example.com" {
		t.Fatalf("admin origins: %v %v", admin, err)
	}
	p, err := c.TrustedProxyPrefixes()
	if err != nil || len(p) != 2 || p[0].String() != "10.0.0.0/8" {
		t.Fatalf("proxies: %v %v", p, err)
	}
}

func TestConsoleDefaultsTrustThePrivateRanges(t *testing.T) {
	t.Setenv("CONSOLE_ADDR", ":8081")
	c, err := Load[Console]()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.TrustedProxyPrefixes()
	if err != nil || len(p) != 6 {
		t.Fatalf("default proxies: %v %v", p, err)
	}
	if !c.RegistrationOpen || c.DeletionGrace != 72*time.Hour {
		t.Fatalf("defaults: registration open %v, grace %s", c.RegistrationOpen, c.DeletionGrace)
	}
}
