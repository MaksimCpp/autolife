package config

import (
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	env := map[string]string{"DB": "postgres://test@localhost/autolife"}
	cfg, err := load(func(k string) string { return env[k] }, "DB")
	if err != nil || cfg.Address != ":8080" || cfg.MaxBodyBytes != 105_000_000 || cfg.DBMaxConns != 10 {
		t.Fatalf("unexpected defaults: %+v, %v", cfg, err)
	}
}

func TestInvalidValues(t *testing.T) {
	for key, value := range map[string]string{
		"DB":                      "password-that-must-not-be-logged",
		"AUTOLIFE_HTTP_ADDR":      ":bad",
		"AUTOLIFE_LOG_LEVEL":      "invalid",
		"AUTOLIFE_MAX_BODY_BYTES": "-1",
		"AUTOLIFE_DB_MAX_CONNS":   "0",
	} {
		t.Run(key, func(t *testing.T) {
			env := map[string]string{"DB": "postgres://test@localhost/autolife"}
			env[key] = value
			_, err := load(func(k string) string { return env[k] }, "DB")
			if err == nil || !strings.Contains(err.Error(), key) || (key == "DB" && strings.Contains(err.Error(), value)) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
