// Package config loads process configuration from the environment, optionally
// seeded from a .env file sitting next to the binary.
package config

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Port          string
	DBPath        string
	AuthUser      string
	AuthPass      string
	NASSSHKeyPath string
	LogLevel      slog.Level
}

// Load reads envPath (ignored if missing) into the environment without
// clobbering variables that are already set, then builds a Config.
func Load(envPath string) (Config, error) {
	if err := loadDotEnv(envPath); err != nil {
		return Config{}, fmt.Errorf("config: reading %s: %w", envPath, err)
	}
	c := Config{
		Port:          env("PORT", "8080"),
		DBPath:        env("DB_PATH", "/data/app.db"),
		AuthUser:      os.Getenv("AUTH_USER"),
		AuthPass:      os.Getenv("AUTH_PASS"),
		NASSSHKeyPath: os.Getenv("NAS_SSH_KEY_PATH"),
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("config: bad LOG_LEVEL %q: %w", os.Getenv("LOG_LEVEL"), err)
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv understands KEY=value lines, # comments and optional quotes.
// ponytail: hand-rolled instead of a dependency; it is 20 lines.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, val); err != nil {
				return err
			}
		}
	}
	return s.Err()
}
