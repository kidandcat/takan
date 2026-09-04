package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Config is runtime configuration from environment.
type Config struct {
	Listen     string
	PublicURL  string // https://atlas.example.com — panel, MCP, OAuth, /install.sh
	DataDir    string
	SessionKey string // cookie signing (and at-rest encryption key material)
	// LocalAddr is the loopback listener serving /health, /jobs, /tasks and
	// /internal/*. The assistant CLIs (atlas-send/-sched/-task) talk to it.
	LocalAddr string
	// AppURL is the host the phone app uses. Only /v1/* is served there.
	// Defaults to PublicURL when unset.
	AppURL string
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

	// --- assistant (the merged Atlas daemon) ---

	// TelegramBotToken bootstraps the assistant's Telegram credential. It is
	// sealed into assistant_meta on first boot; the env value always wins.
	TelegramBotToken string
	// OwnerTelegramID is the owner's Telegram *user* id. In a private chat it
	// equals the chat id, so it doubles as the default DM target.
	OwnerTelegramID int64
	// GroqAPIKey enables voice-note transcription. Optional.
	GroqAPIKey string
	// AppToken is the phone app's bearer token for /v1/*. Optional.
	AppToken string
	// FirebaseServiceAccount is the FCM service account JSON. Empty leaves push
	// off; the app still works over SSE in the foreground.
	FirebaseServiceAccount string
	// AgentHome is where the CLI agent's home lives (~/.grok). Defaults to $HOME.
	AgentHome string
	// LegacyDir is the one-shot import source for the standalone daemon's JSON
	// state and config.toml. Empty disables the import.
	LegacyDir string

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
		Listen:                 env2("ATLAS_LISTEN", "TAKAN_LISTEN", "127.0.0.1:8090"),
		PublicURL:              strings.TrimRight(env2("ATLAS_PUBLIC_URL", "TAKAN_PUBLIC_URL", "http://127.0.0.1:8090"), "/"),
		DataDir:                env2("ATLAS_DATA_DIR", "TAKAN_DATA_DIR", "./data"),
		SessionKey:             env2("ATLAS_SESSION_KEY", "TAKAN_SESSION_KEY", ""),
		LocalAddr:              env2("ATLAS_LOCAL_ADDR", "TAKAN_LOCAL_ADDR", "127.0.0.1:8099"),
		AppURL:                 strings.TrimRight(env2("ATLAS_APP_URL", "TAKAN_APP_URL", ""), "/"),
		MachineBashPerMin:      envInt2("ATLAS_MACHINE_BASH_PER_MIN", "TAKAN_MACHINE_BASH_PER_MIN", 30),
		AuthPerMin:             envInt2("ATLAS_AUTH_PER_MIN", "TAKAN_AUTH_PER_MIN", 20),
		OwnerEmail:             strings.ToLower(strings.TrimSpace(env2("ATLAS_OWNER_EMAIL", "TAKAN_OWNER_EMAIL", ""))),
		AuthEmailFrom:          strings.TrimSpace(env2("ATLAS_AUTH_EMAIL_FROM", "TAKAN_AUTH_EMAIL_FROM", "")),
		ResendAPIKey:           strings.TrimSpace(env2("ATLAS_RESEND_API_KEY", "TAKAN_RESEND_API_KEY", "")),
		LoginCodePerWindow:     envInt2("ATLAS_LOGIN_CODE_PER_WINDOW", "TAKAN_LOGIN_CODE_PER_WINDOW", 3),
		LoginCodeWindowMin:     envInt2("ATLAS_LOGIN_CODE_WINDOW_MIN", "TAKAN_LOGIN_CODE_WINDOW_MIN", 15),
		TelegramBotToken:       strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		OwnerTelegramID:        envInt64("OWNER_TELEGRAM_ID", 0),
		GroqAPIKey:             strings.TrimSpace(os.Getenv("GROQ_API_KEY")),
		AppToken:               strings.TrimSpace(env2("ATLAS_APP_TOKEN", "TAKAN_APP_TOKEN", "")),
		FirebaseServiceAccount: strings.TrimSpace(os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON")),
		AgentHome:              strings.TrimSpace(env2("ATLAS_AGENT_HOME", "TAKAN_AGENT_HOME", os.Getenv("HOME"))),
		LegacyDir:              strings.TrimSpace(env2("ATLAS_LEGACY_DIR", "TAKAN_LEGACY_DIR", "")),
		BackupEndpoint:         env2("ATLAS_BACKUP_ENDPOINT", "TAKAN_BACKUP_ENDPOINT", ""),
		BackupRegion:           env2("ATLAS_BACKUP_REGION", "TAKAN_BACKUP_REGION", "gra"),
		BackupBucket:           env2("ATLAS_BACKUP_BUCKET", "TAKAN_BACKUP_BUCKET", ""),
		BackupPrefix:           env2("ATLAS_BACKUP_PREFIX", "TAKAN_BACKUP_PREFIX", "takan/"),
		BackupAccessKey:        os.Getenv("AWS_ACCESS_KEY_ID"),
		BackupSecretKey:        os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if c.SessionKey == "" {
		c.SessionKey = "dev-insecure-change-me"
	}
	if c.AppURL == "" {
		c.AppURL = c.PublicURL
	}
	return c
}

// deprecatedOnce keeps the TAKAN_* fallback warning to one line per variable,
// so a long-lived process does not spam the journal.
var deprecatedOnce sync.Map

// env2 reads the preferred name first, then the deprecated one. Using the
// deprecated name logs a single warning so an env file can be migrated
// independently of the binary.
func env2(preferred, deprecated, def string) string {
	if v := os.Getenv(preferred); v != "" {
		return v
	}
	if v := os.Getenv(deprecated); v != "" {
		if _, seen := deprecatedOnce.LoadOrStore(deprecated, true); !seen {
			log.Printf("config: %s is deprecated, rename it to %s", deprecated, preferred)
		}
		return v
	}
	return def
}

func envInt2(preferred, deprecated string, def int) int {
	v := env2(preferred, deprecated, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envInt64(k string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("config: %s is not a valid integer (%q), ignoring", k, v)
		return def
	}
	return n
}
