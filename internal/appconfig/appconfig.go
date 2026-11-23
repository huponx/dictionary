// Package appconfig is the environment configuration of the dictionary service.
package appconfig

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/huponx/gocommon/config"
)

// Dictionary is the configuration of the gRPC server. Every variable has the prefix DICTIONARY_ except
// the shared blocks of gocommon (HOST, PORT, LOG_*, POSTGRES_*).
type Dictionary struct {
	config.Log
	config.Postgres

	// gRPC listen address (HOST and PORT are the same variables as in the other services).
	Host            string        `env:"HOST" envDefault:"0.0.0.0"`
	Port            int           `env:"PORT" envDefault:"9090"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`

	// Issuer is the iss of the tokens identity issues, JWKSURL is where identity publishes its public keys
	// (inside the cluster: http://identity.application.svc.cluster.local:8080/.well-known/jwks.json) and
	// AuthAudience the aud the tokens must carry (the exchange policy gatekeeper -> dictionary issues them).
	Issuer       string `env:"DICTIONARY_ISSUER,required"`
	JWKSURL      string `env:"DICTIONARY_JWKS_URL,required"`
	AuthAudience string `env:"DICTIONARY_AUTH_AUDIENCE" envDefault:"dictionary"`

	// The preferences of a user live in identity. Dictionary calls it on behalf of the user: it exchanges the
	// token of the request for one for the audience IdentityAudience (RFC 8693, the identity client below
	// needs an exchange policy to that audience) and calls IdentityAddr (gRPC). Without IdentityAddr the
	// preferences are the defaults and cannot be changed.
	IdentityAddr     string `env:"DICTIONARY_IDENTITY_ADDR"`
	IdentityInsecure bool   `env:"DICTIONARY_IDENTITY_INSECURE" envDefault:"true"`
	IdentityAudience string `env:"DICTIONARY_IDENTITY_AUDIENCE" envDefault:"identity"`
	TokenURL         string `env:"DICTIONARY_TOKEN_URL"`
	ClientID         string `env:"DICTIONARY_CLIENT_ID"`
	ClientSecret     string `env:"DICTIONARY_CLIENT_SECRET"`

	// MaxSavedWords is the default of the setting "max saved words" (an administrator changes it in the
	// application; the value stored there wins): the number of pending (not yet exported) words a user may keep.
	MaxSavedWords int `env:"DICTIONARY_MAX_SAVED_WORDS" envDefault:"100"`
	// ExportMaxBytes caps the size of an exported .apkg. It is also the gRPC message limit (plus a margin),
	// and gatekeeper has to accept a response that large.
	ExportMaxBytes int `env:"DICTIONARY_EXPORT_MAX_BYTES" envDefault:"3145728"`
	// DeckLinkTTL is how long a download link of a deck works (it works once).
	DeckLinkTTL time.Duration `env:"DICTIONARY_DECK_LINK_TTL" envDefault:"15m"`
	// ExportedRetention is how long exported rows are kept before the cleanup deletes them.
	ExportedRetention time.Duration `env:"DICTIONARY_EXPORTED_RETENTION" envDefault:"168h"`
	// CleanupInterval is the period of that cleanup.
	CleanupInterval time.Duration `env:"DICTIONARY_CLEANUP_INTERVAL" envDefault:"1h"`

	// LookupCacheTTL and LookupCacheSize bound the in-memory cache of lookups (the free APIs are not hammered).
	LookupCacheTTL  time.Duration `env:"DICTIONARY_LOOKUP_CACHE_TTL" envDefault:"24h"`
	LookupCacheSize int           `env:"DICTIONARY_LOOKUP_CACHE_SIZE" envDefault:"2000"`
	// HTTPTimeout is the timeout of one call to a dictionary provider.
	HTTPTimeout time.Duration `env:"DICTIONARY_HTTP_TIMEOUT" envDefault:"10s"`
	// UserAgent is sent to the providers (Wikimedia asks for a name and a contact).
	UserAgent string `env:"DICTIONARY_USER_AGENT" envDefault:"huponx-dictionary/1.0 (+https://github.com/huponx/dictionary)"`
	// ViFallbackMT turns on the machine translation (MyMemory) of the English definitions when the
	// Vietnamese Wiktionary has no entry for a word.
	ViFallbackMT bool `env:"DICTIONARY_VI_FALLBACK_MT" envDefault:"true"`

	// Base URLs of the providers (overridden in tests).
	FreeDictURL   string `env:"DICTIONARY_FREEDICT_URL" envDefault:"https://api.dictionaryapi.dev/api/v2/entries/en"`
	WiktionaryURL string `env:"DICTIONARY_WIKTIONARY_URL" envDefault:"https://vi.wiktionary.org/w/api.php"`
	MyMemoryURL   string `env:"DICTIONARY_MYMEMORY_URL" envDefault:"https://api.mymemory.translated.net/get"`

	// AudioHosts are the hosts a saved audio URL may point to: the service downloads them when it builds
	// a deck, so an open list would let a caller make it fetch any address. One entry per line or comma.
	AudioHosts []string `env:"DICTIONARY_AUDIO_HOSTS" envSeparator:"," envDefault:"api.dictionaryapi.dev,upload.wikimedia.org,ssl.gstatic.com"`
}

// Addr is the gRPC listen address.
func (c Dictionary) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// IsLocal reports a local development run (reflection on, http JWKS allowed).
func (c Dictionary) IsLocal() bool { return strings.EqualFold(c.Env, "local") }

// Hosts returns the allowed audio hosts, lower case.
func (c Dictionary) Hosts() []string {
	var out []string
	for _, h := range config.Fields(c.AudioHosts) {
		out = append(out, strings.ToLower(h))
	}
	return out
}

// Validate rejects settings that would start the service unsafely or uselessly.
func (c Dictionary) Validate() error {
	if err := config.ValidatePort(c.Port); err != nil {
		return err
	}
	if strings.TrimSpace(c.AuthAudience) == "" {
		return fmt.Errorf("DICTIONARY_AUTH_AUDIENCE must not be blank")
	}
	if !c.IsLocal() && !strings.HasPrefix(c.Issuer, "https://") {
		return fmt.Errorf("DICTIONARY_ISSUER must be https outside ENV=local")
	}
	if u, err := url.Parse(c.JWKSURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("DICTIONARY_JWKS_URL must be an http(s) URL, got %q", c.JWKSURL)
	}
	if c.IdentityAddr != "" {
		if strings.TrimSpace(c.IdentityAudience) == "" || strings.TrimSpace(c.ClientID) == "" ||
			strings.TrimSpace(c.ClientSecret) == "" {
			return fmt.Errorf("DICTIONARY_IDENTITY_ADDR needs DICTIONARY_CLIENT_ID, DICTIONARY_CLIENT_SECRET and DICTIONARY_IDENTITY_AUDIENCE")
		}
		if u, err := url.Parse(c.TokenURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("DICTIONARY_TOKEN_URL must be an http(s) URL, got %q", c.TokenURL)
		}
	}
	if c.MaxSavedWords < 1 || c.MaxSavedWords > 1000 {
		return fmt.Errorf("DICTIONARY_MAX_SAVED_WORDS must be between 1 and 1000")
	}
	if c.ExportMaxBytes < 1<<16 {
		return fmt.Errorf("DICTIONARY_EXPORT_MAX_BYTES must be at least 65536")
	}
	if c.DeckLinkTTL < time.Minute || c.DeckLinkTTL > 24*time.Hour {
		return fmt.Errorf("DICTIONARY_DECK_LINK_TTL must be between 1m and 24h")
	}
	if c.ExportedRetention < time.Hour {
		return fmt.Errorf("DICTIONARY_EXPORTED_RETENTION must be at least 1h")
	}
	if c.CleanupInterval < time.Minute {
		return fmt.Errorf("DICTIONARY_CLEANUP_INTERVAL must be at least 1m")
	}
	if c.LookupCacheTTL < 0 || c.LookupCacheSize < 0 {
		return fmt.Errorf("the lookup cache settings must not be negative")
	}
	if len(c.Hosts()) == 0 {
		return fmt.Errorf("DICTIONARY_AUDIO_HOSTS must list at least one host")
	}
	return nil
}
