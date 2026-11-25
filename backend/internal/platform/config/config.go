package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
)

type Config struct {
	Address      string
	DatabaseURL  string
	MaxBodyBytes int64
	DBMaxConns   int32
	LogLevel     slog.Level
}

func Load(databaseKey string) (Config, error) {
	return load(os.Getenv, databaseKey)
}

func load(env func(string) string, databaseKey string) (Config, error) {
	cfg := Config{Address: ":8080", MaxBodyBytes: 105_000_000, DBMaxConns: 10}
	cfg.DatabaseURL = env(databaseKey)
	dsn, err := url.Parse(cfg.DatabaseURL)
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") ||
		dsn.Hostname() == "" || dsn.Path == "" || dsn.Path == "/" || dsn.User == nil || dsn.User.Username() == "" {
		return Config{}, fmt.Errorf("%s must be a PostgreSQL URL with a host, user and database", databaseKey)
	}
	if address := env("AUTOLIFE_HTTP_ADDR"); address != "" {
		cfg.Address = address
	}
	_, port, err := net.SplitHostPort(cfg.Address)
	if number, parseErr := strconv.ParseUint(port, 10, 16); err != nil || parseErr != nil || number == 0 {
		return Config{}, fmt.Errorf("AUTOLIFE_HTTP_ADDR must contain a valid TCP port")
	}
	if level := env("AUTOLIFE_LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("AUTOLIFE_LOG_LEVEL must be debug, info, warn or error")
		}
	}
	if value := env("AUTOLIFE_MAX_BODY_BYTES"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 || n > 105_000_000 {
			return Config{}, fmt.Errorf("AUTOLIFE_MAX_BODY_BYTES must be in 1..105000000")
		}
		cfg.MaxBodyBytes = n
	}
	if value := env("AUTOLIFE_DB_MAX_CONNS"); value != "" {
		n, err := strconv.ParseInt(value, 10, 32)
		if err != nil || n < 1 || n > 100 {
			return Config{}, fmt.Errorf("AUTOLIFE_DB_MAX_CONNS must be in 1..100")
		}
		cfg.DBMaxConns = int32(n)
	}
	return cfg, nil
}

func Logger(service string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", service)
}

func (c Config) Logger(service string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: c.LogLevel})).With("service", service)
}
