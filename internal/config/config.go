package config

import (
	"os"
	"strconv"
	"strings"
)

// Config is runtime configuration from environment.
type Config struct {
	Listen     string
	PublicURL  string // https://takan.example.com — panel, MCP, OAuth, /install.sh
	DataDir    string
	SessionKey string // cookie signing (and at-rest encryption key material)
	// MachineBashPerMin rate-limits machine_bash per user (0 = unlimited).
	MachineBashPerMin int
	// AuthPerMin rate-limits login / OAuth token / API login per IP.
	AuthPerMin int
	// OwnerEmail receives the panel login codes. Login fails closed when this
	// is empty and the database has no usable owner address.
	OwnerEmail string
	// AuthEmailFrom overrides the From address of login-code mails
	// (default: login@<first Resend domain configured for the owner>).
	AuthEmailFrom string
	// ResendAPIKey lets a fresh instance send its first login code before any
	// owner row or panel configuration exists. Optional afterwards.
	ResendAPIKey string
	// LoginCodePerWindow caps "send code" requests per IP and globally within
	// LoginCodeWindowMin minutes (0 = unlimited).
	LoginCodePerWindow int
	LoginCodeWindowMin int

	// Optional Colmena S3 backup
	BackupEndpoint  string
	BackupRegion    string
	BackupBucket    string
	BackupPrefix    string
	BackupAccessKey string
	BackupSecretKey string
}

func Load() Config {
	c := Config{
		Listen:             env("TAKAN_LISTEN", "127.0.0.1:8090"),
		PublicURL:          strings.TrimRight(env("TAKAN_PUBLIC_URL", "http://127.0.0.1:8090"), "/"),
		DataDir:            env("TAKAN_DATA_DIR", "./data"),
		SessionKey:         env("TAKAN_SESSION_KEY", ""),
		MachineBashPerMin:  EnvInt("TAKAN_MACHINE_BASH_PER_MIN", 30),
		AuthPerMin:         EnvInt("TAKAN_AUTH_PER_MIN", 20),
		OwnerEmail:         strings.ToLower(strings.TrimSpace(os.Getenv("TAKAN_OWNER_EMAIL"))),
		AuthEmailFrom:      strings.TrimSpace(os.Getenv("TAKAN_AUTH_EMAIL_FROM")),
		ResendAPIKey:       strings.TrimSpace(os.Getenv("TAKAN_RESEND_API_KEY")),
		LoginCodePerWindow: EnvInt("TAKAN_LOGIN_CODE_PER_WINDOW", 3),
		LoginCodeWindowMin: EnvInt("TAKAN_LOGIN_CODE_WINDOW_MIN", 15),
		BackupEndpoint:     os.Getenv("TAKAN_BACKUP_ENDPOINT"),
		BackupRegion:       env("TAKAN_BACKUP_REGION", "gra"),
		BackupBucket:       os.Getenv("TAKAN_BACKUP_BUCKET"),
		BackupPrefix:       env("TAKAN_BACKUP_PREFIX", "takan/"),
		BackupAccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		BackupSecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if c.SessionKey == "" {
		c.SessionKey = "dev-insecure-change-me"
	}
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func EnvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
