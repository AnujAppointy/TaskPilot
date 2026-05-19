package taskpilot

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

type ServerConfig struct {
	Addr        string
	DBPath      string
	Token       string
	SecretKey   string
	BaseURL     string
	ArtifactDir string
	Production  bool
}

func LoadServerConfig(addrFlag, dbFlag, tokenFlag string, production bool) ServerConfig {
	addr := firstNonEmpty(addrFlag, os.Getenv("TASKPILOT_HTTP_ADDR"), "127.0.0.1:8080")
	db := firstNonEmpty(os.Getenv("TASKPILOT_DB_URL"), dbFlag, "taskpilot.db")
	token := firstNonEmpty(tokenFlag, os.Getenv("TASKPILOT_TOKEN"), "dev-token")
	return ServerConfig{
		Addr:        addr,
		DBPath:      db,
		Token:       token,
		SecretKey:   os.Getenv("TASKPILOT_SECRET_KEY"),
		BaseURL:     os.Getenv("TASKPILOT_BASE_URL"),
		ArtifactDir: firstNonEmpty(os.Getenv("TASKPILOT_ARTIFACT_DIR"), "artifacts"),
		Production:  production || strings.EqualFold(os.Getenv("TASKPILOT_ENV"), "production"),
	}
}

func (c ServerConfig) Validate() error {
	host, _, _ := net.SplitHostPort(c.Addr)
	exposed := host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1"
	if c.Token == "" || c.Token == "dev-token" {
		if exposed || c.Production {
			return userErr("validation", "refusing to expose server with default token; set TASKPILOT_TOKEN or pass --token with a strong value")
		}
	}
	if c.Production && len(c.SecretKey) < 32 {
		return userErr("validation", "production mode requires TASKPILOT_SECRET_KEY with at least 32 characters")
	}
	return nil
}

func (c ServerConfig) EffectiveBaseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return fmt.Sprintf("http://%s", c.Addr)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func heartbeatInterval() time.Duration {
	if v := os.Getenv("TASKPILOT_HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}
