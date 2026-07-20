package config

import (
	"os"
	"strconv"
	"strings"
)

// Config is runtime configuration from environment.
type Config struct {
	Listen     string
	PublicURL  string // https://takan.es
	DataDir    string
	SessionKey string // cookie signing
	// AllowRegister enables public self-signup without invite. Default false = invite required.
	AllowRegister bool
	// DefaultInviteQuota is how many invites a new user may create (unless unlimited).
	DefaultInviteQuota int
	// OAuthRedirectExtra hosts added to the default OAuth redirect allowlist (comma-separated).
	OAuthRedirectExtra []string
	// MachineBashPerMin rate-limits machine_bash per user (0 = unlimited).
	MachineBashPerMin int
	// AuthPerMin rate-limits login/register/token per IP.
	AuthPerMin int
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
		AllowRegister:      envBool("TAKAN_ALLOW_REGISTER", false),
		DefaultInviteQuota: EnvInt("TAKAN_DEFAULT_INVITE_QUOTA", 5),
		OAuthRedirectExtra: splitCSV(os.Getenv("TAKAN_OAUTH_REDIRECT_ALLOW")),
		MachineBashPerMin:  EnvInt("TAKAN_MACHINE_BASH_PER_MIN", 30),
		AuthPerMin:         EnvInt("TAKAN_AUTH_PER_MIN", 20),
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
	if c.DefaultInviteQuota < 0 {
		c.DefaultInviteQuota = 0
	}
	return c
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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

// envBool parses common truthy/falsey strings; empty uses def.
func envBool(k string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
