package appconfig

import (
	"strings"
	"testing"
	"time"

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
	if cfg.FilesDir != "" || cfg.AudioMaxBytes != 400<<10 || cfg.AudioUserQuota != 10<<20 || cfg.ImageMaxBytes != 300<<10 || cfg.ImageMaxDim != 1024 || cfg.ImageMaxPixels != 25_000_000 || cfg.ImageUserQuota != 20<<20 {
		t.Errorf("image defaults: %+v", cfg)
	}
	if cfg.UnusedFileAge != 24*time.Hour || cfg.OrphanSweepEvery != 24*time.Hour || cfg.OrphanFileAge != time.Hour {
		t.Errorf("cleanup defaults: %+v", cfg)
	}
	if got := strings.Join(cfg.Hosts(), " "); got != "api.dictionaryapi.dev upload.wikimedia.org ssl.gstatic.com dictionary.cambridge.org" {
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
		"http issuer outside local":  {"DICTIONARY_ISSUER": "http://auth.example.com"},
		"blank audience":             {"DICTIONARY_AUTH_AUDIENCE": " "},
		"bad jwks url":               {"DICTIONARY_JWKS_URL": "identity/jwks"},
		"limit zero":                 {"DICTIONARY_MAX_SAVED_WORDS": "0"},
		"limit over a million":       {"DICTIONARY_MAX_SAVED_WORDS": "1000001"},
		"tiny export limit":          {"DICTIONARY_EXPORT_MAX_BYTES": "10"},
		"retention under an hour":    {"DICTIONARY_EXPORTED_RETENTION": "10m"},
		"cleanup too often":          {"DICTIONARY_CLEANUP_INTERVAL": "1s"},
		"port out of range":          {"PORT": "70000"},
		"image bytes over the body":  {"DICTIONARY_IMAGE_MAX_BYTES": "900000"},
		"image dim tiny":             {"DICTIONARY_IMAGE_MAX_DIM": "10"},
		"image pixels huge":          {"DICTIONARY_IMAGE_MAX_PIXELS": "1000000000"},
		"quota under one image":      {"DICTIONARY_IMAGE_USER_QUOTA": "1000"},
		"audio bytes over the body":  {"DICTIONARY_AUDIO_MAX_BYTES": "900000"},
		"audio bytes tiny":           {"DICTIONARY_AUDIO_MAX_BYTES": "100"},
		"quota under one audio":      {"DICTIONARY_AUDIO_USER_QUOTA": "1000"},
		"quota over its ceiling":     {"DICTIONARY_IMAGE_USER_QUOTA_CEILING": "1048576"},
		"content quota over ceiling": {"DICTIONARY_CONTENT_USER_QUOTA_CEILING": "1000000"},
		"content ceiling too small":  {"DICTIONARY_CONTENT_MAX_BYTES_CEILING": "1000"},
		"content ceiling over body":  {"DICTIONARY_CONTENT_MAX_BYTES_CEILING": "900000"},
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

func TestMaxSavedWordsUpToAMillion(t *testing.T) {
	env(t, map[string]string{
		"ENV": "production", "DICTIONARY_ISSUER": "https://auth.example.com", "DICTIONARY_JWKS_URL": "https://auth.example.com/jwks",
		"DICTIONARY_MAX_SAVED_WORDS": "1000000",
	})
	cfg, err := config.Load[Dictionary]()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil || cfg.MaxSavedWords != 1_000_000 {
		t.Errorf("a million words: %v %d", err, cfg.MaxSavedWords)
	}
}

func TestWorkerNeedsNoIdentity(t *testing.T) {
	env(t, map[string]string{"ENV": "production"})
	cfg, err := config.Load[Worker]()
	if err != nil {
		t.Fatalf("a worker without DICTIONARY_ISSUER and DICTIONARY_JWKS_URL: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.HealthAddr != ":8081" || cfg.ShutdownTimeout != 15*time.Second || cfg.ExportPartMaxBytesCeiling != 50<<20 {
		t.Errorf("worker defaults: %+v", cfg)
	}
	if got := strings.Join(cfg.Hosts(), " "); got != "api.dictionaryapi.dev upload.wikimedia.org ssl.gstatic.com dictionary.cambridge.org" {
		t.Errorf("hosts = %q", got)
	}
}

func TestSharedLimitsRejectedByBothStructs(t *testing.T) {
	for name, change := range map[string]map[string]string{
		"part ceiling under the default":   {"DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING": "1048576"},
		"export ceiling under the default": {"DICTIONARY_EXPORT_MAX_BYTES_CEILING": "1048576"},
		"batch too small":                  {"DICTIONARY_EXPORT_JOB_BATCH_MAX": "10"},
		"job ttl under an hour":            {"DICTIONARY_EXPORT_JOB_TTL": "10m"},
		"cleanup too often":                {"DICTIONARY_CLEANUP_INTERVAL": "1s"},
		"no audio hosts":                   {"DICTIONARY_AUDIO_HOSTS": " "},
	} {
		t.Run(name, func(t *testing.T) {
			env(t, map[string]string{
				"ENV": "production", "DICTIONARY_ISSUER": "https://auth.example.com", "DICTIONARY_JWKS_URL": "https://auth.example.com/jwks",
			})
			env(t, change)
			api, err := config.Load[Dictionary]()
			if err != nil {
				t.Fatal(err)
			}
			if api.Validate() == nil {
				t.Error("Dictionary.Validate accepted it")
			}
			w, err := config.Load[Worker]()
			if err != nil {
				t.Fatal(err)
			}
			if w.Validate() == nil {
				t.Error("Worker.Validate accepted it")
			}
		})
	}
}
