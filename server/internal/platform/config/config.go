// Package config loads settings from environment variables.
//
// Settings are split per dependency so that each subcommand only parses (and requires) what it uses:
// `migrate` never asks for Redis credentials, `api` never asks for RabbitMQ.
package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Server holds the listen addresses and timeouts shared by every long-running subcommand.
type Server struct {
	// HTTPAddr is the public listener (REST + WebSocket). Only used by `api`.
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// ManagementAddr serves /healthz, /readyz and /metrics. Never exposed through the ingress.
	ManagementAddr  string        `env:"MANAGEMENT_ADDR" envDefault:":9090"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"20s"`
	// ShutdownDelay is how long the process keeps serving after SIGTERM while /readyz already fails, so that
	// Kubernetes and the ingress stop routing here first. The image has no shell, so this replaces a preStop sleep.
	ShutdownDelay time.Duration `env:"SHUTDOWN_DELAY" envDefault:"0s"`
	LogLevel      string        `env:"LOG_LEVEL" envDefault:"info"`
}

// Postgres is reached through pgbouncer in transaction mode, see db.Open.
type Postgres struct {
	Host     string `env:"POSTGRES_HOST,required"`
	Port     int    `env:"POSTGRES_PORT" envDefault:"5432"`
	Database string `env:"POSTGRES_DATABASE,required"`
	User     string `env:"POSTGRES_USER,required"`
	Password string `env:"POSTGRES_PASSWORD,required"`
	SSLMode  string `env:"POSTGRES_SSLMODE" envDefault:"disable"`
	MaxConns int32  `env:"POSTGRES_MAX_CONNS" envDefault:"10"`
}

// DSN returns a libpq style URL. The password is escaped, so any character is allowed.
func (p Postgres) DSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(p.User, p.Password),
		Host:   fmt.Sprintf("%s:%d", p.Host, p.Port),
		Path:   "/" + p.Database,
	}
	q := url.Values{}
	q.Set("sslmode", p.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

type Redis struct {
	Host     string `env:"REDIS_HOST,required"`
	Port     int    `env:"REDIS_PORT" envDefault:"6379"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
	// KeyPrefix is put in front of every key AND every Pub/Sub channel. Pub/Sub ignores the DB index,
	// so the prefix is what keeps Marpokat apart from other apps on a shared Redis (PLAN D20).
	KeyPrefix string `env:"REDIS_KEY_PREFIX" envDefault:"marpokat:"`
}

func (r Redis) Addr() string { return fmt.Sprintf("%s:%d", r.Host, r.Port) }

type RabbitMQ struct {
	Host        string `env:"RABBITMQ_HOST,required"`
	Port        int    `env:"RABBITMQ_PORT" envDefault:"5672"`
	Username    string `env:"RABBITMQ_USERNAME,required"`
	Password    string `env:"RABBITMQ_PASSWORD,required"`
	VirtualHost string `env:"RABBITMQ_VIRTUAL_HOST" envDefault:"/marpokat-dev"`
}

// URL returns the AMQP URL. The vhost is path-escaped ("/marpokat" becomes "%2Fmarpokat").
func (r RabbitMQ) URL() string {
	u := url.URL{
		Scheme:  "amqp",
		User:    url.UserPassword(r.Username, r.Password),
		Host:    fmt.Sprintf("%s:%d", r.Host, r.Port),
		Path:    "/" + r.VirtualHost,
		RawPath: "/" + url.PathEscape(r.VirtualHost),
	}
	return u.String()
}

type MinIO struct {
	// Endpoint is a full URL, e.g. https://infra-minio.rizkysiregar.my.id/ (the scheme decides TLS).
	Endpoint  string `env:"MINIO_ENDPOINT,required"`
	AccessKey string `env:"MINIO_ACCESS_KEY,required"`
	SecretKey string `env:"MINIO_SECRET_KEY,required"`
	Bucket    string `env:"MINIO_BUCKET,required"`
	// Region is only used to sign links; naming it spares a request to the server for every link we hand out.
	Region string `env:"MINIO_REGION" envDefault:"us-east-1"`
}

// HostAndTLS splits Endpoint into the host[:port] minio-go wants and whether to use TLS.
func (m MinIO) HostAndTLS() (string, bool, error) {
	u, err := url.Parse(m.Endpoint)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("MINIO_ENDPOINT %q is not a URL like https://host[:port]", m.Endpoint)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("MINIO_ENDPOINT scheme must be http or https, got %q", u.Scheme)
	}
}

// Console is the account side of the website (PLAN M9). It is off unless Addr is set: the Console API lives on its own
// listener, reachable only from the website inside the cluster, never through the public ingress.
type Console struct {
	Addr string `env:"CONSOLE_ADDR"`
	// ServiceKey is what the website sends in X-Console-Key. At least 32 characters.
	ServiceKey string `env:"CONSOLE_SERVICE_KEY"`
	// BaseURL is the public address of the website, for the links in e-mails.
	BaseURL     string `env:"CONSOLE_BASE_URL"`
	MaxProjects int    `env:"CONSOLE_MAX_PROJECTS" envDefault:"3"`

	// MailDriver is "log" (prints the e-mail, development) or "smtp".
	MailDriver   string `env:"MAIL_DRIVER" envDefault:"log"`
	MailHost     string `env:"MAIL_HOST"`
	MailPort     int    `env:"MAIL_PORT" envDefault:"587"`
	MailUsername string `env:"MAIL_USERNAME"`
	MailPassword string `env:"MAIL_PASSWORD"`
	MailFrom     string `env:"MAIL_FROM"`
}

func (c Console) Enabled() bool { return c.Addr != "" }

// Validate checks what a running Console API needs. A weak service key would make the whole account system
// guessable, so it fails the start rather than running.
func (c Console) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if len(c.ServiceKey) < 32 {
		return fmt.Errorf("CONSOLE_SERVICE_KEY must be at least 32 characters (generate with: openssl rand -base64 32)")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("CONSOLE_BASE_URL %q is not a URL like https://marpokat.example.com", c.BaseURL)
	}
	switch c.MailDriver {
	case "log":
	case "smtp":
		if c.MailHost == "" || c.MailFrom == "" {
			return fmt.Errorf("MAIL_DRIVER=smtp needs MAIL_HOST and MAIL_FROM")
		}
	default:
		return fmt.Errorf("MAIL_DRIVER must be log or smtp, got %q", c.MailDriver)
	}
	return nil
}

// Secrets holds the master key that encrypts tenant secrets at rest (AES-256-GCM, PLAN section 6).
type Secrets struct {
	MasterKey string `env:"MARPOKAT_MASTER_KEY,required"`
}

// MasterKeyBytes decodes the base64 master key and checks it is 32 bytes (AES-256).
func (s Secrets) MasterKeyBytes() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s.MasterKey))
	if err != nil {
		return nil, fmt.Errorf("MARPOKAT_MASTER_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("MARPOKAT_MASTER_KEY must decode to 32 bytes, got %d (generate with: openssl rand -base64 32)", len(key))
	}
	return key, nil
}

// Load parses the environment into any of the structs above.
func Load[T any]() (T, error) {
	var c T
	if err := env.Parse(&c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	return c, nil
}
