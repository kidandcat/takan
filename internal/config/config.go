package config

import (
	"os"
	"strconv"
	"strings"
)

// Config is runtime configuration from environment.
type Config struct {
	Listen      string
	PublicURL   string // https://takan.es
	DataDir     string
	SessionKey  string // cookie signing
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
		Listen:    env("TAKAN_LISTEN", "127.0.0.1:8090"),
		PublicURL: strings.TrimRight(env("TAKAN_PUBLIC_URL", "http://127.0.0.1:8090"), "/"),
		DataDir:   env("TAKAN_DATA_DIR", "./data"),
		SessionKey: env("TAKAN_SESSION_KEY", ""),
		BackupEndpoint:  os.Getenv("TAKAN_BACKUP_ENDPOINT"),
		BackupRegion:    env("TAKAN_BACKUP_REGION", "gra"),
		BackupBucket:    os.Getenv("TAKAN_BACKUP_BUCKET"),
		BackupPrefix:    env("TAKAN_BACKUP_PREFIX", "takan/"),
		BackupAccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		BackupSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
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
