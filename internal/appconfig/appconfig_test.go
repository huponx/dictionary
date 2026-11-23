package appconfig

import (
	"strings"
	"testing"

	"github.com/huponx/gocommon/config"
)

func env(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaultsAndValidate(t *testing.T) {
	env(t, map[string]string{
		"ENV": "production", "DICTIONARY_ISSUER": "https://auth.example.com",
		"DICTIONARY_JWKS_URL": "http://identity:8080/.well-known/jwks.json",
	})
	cfg, err := config.Load[Dictionary]()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr() != "0.0.0.0:9090" || cfg.AuthAudience != "dictionary" || cfg.MaxSavedWords != 100 || !cfg.ViFallbackMT {
		t.Errorf("defaults: %+v", cfg)
	}
	if got := strings.Join(cfg.Hosts(), " "); got != "api.dictionaryapi.dev upload.wikimedia.org ssl.gstatic.com" {
		t.Errorf("hosts = %q", got)
	}
}

func TestAudioHostsOnePerLine(t *testing.T) {
	env(t, map[string]string{
		"DICTIONARY_ISSUER": "http://localhost:8080", "ENV": "local", "DICTIONARY_JWKS_URL": "http://localhost:8080/jwks",
		"DICTIONARY_AUDIO_HOSTS": "A.example.com\nb.example.com, C.example.com",
	})
	cfg, _ := config.Load[Dictionary]()
	if got := strings.Join(cfg.Hosts(), " "); got != "a.example.com b.example.com c.example.com" {
		t.Errorf("hosts = %q", got)
	}
}

func TestValidateRejects(t *testing.T) {
	good := map[string]string{
		"ENV": "production", "DICTIONARY_ISSUER": "https://auth.example.com", "DICTIONARY_JWKS_URL": "https://auth.example.com/jwks",
	}
	for name, change := range map[string]map[string]string{
		"http issuer outside local": {"DICTIONARY_ISSUER": "http://auth.example.com"},
		"blank audience":            {"DICTIONARY_AUTH_AUDIENCE": " "},
		"bad jwks url":              {"DICTIONARY_JWKS_URL": "identity/jwks"},
		"limit zero":                {"DICTIONARY_MAX_SAVED_WORDS": "0"},
		"tiny export limit":         {"DICTIONARY_EXPORT_MAX_BYTES": "10"},
		"retention under an hour":   {"DICTIONARY_EXPORTED_RETENTION": "10m"},
		"cleanup too often":         {"DICTIONARY_CLEANUP_INTERVAL": "1s"},
		"port out of range":         {"PORT": "70000"},
	} {
		t.Run(name, func(t *testing.T) {
			env(t, good)
			env(t, change)
			cfg, err := config.Load[Dictionary]()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
}
