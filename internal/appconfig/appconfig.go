// Package appconfig is the environment configuration of the dictionary service.
package appconfig

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/gocommon/config"
)

// Storage is what the API and the worker share: the volume of the files, the limits of the files and of the
// exports and the settings of the cleanup. Both processes read the same variables, so a value (for example the
// ceiling of the part size) has to be the same in both.
type Storage struct {
	// MaxSavedWords is the default of the setting "max saved words" (an administrator changes it in the
	// application; the value stored there wins): the number of pending (not yet exported) words a user may keep.
	MaxSavedWords int `env:"DICTIONARY_MAX_SAVED_WORDS" envDefault:"100"`
	// ExportMaxBytes is the default of the system setting "export size": the largest .apkg of a synchronous export
	// (ExportAnki, CreateDeckLink), which travels in one gRPC message and as base64 in JSON.
	// ExportMaxBytesCeiling is the most an administrator can set (it is also the gRPC message limit, plus a
	// margin, and gatekeeper has to accept a response that large: GATEKEEPER_UPSTREAM_MAX_RECV_BYTES).
	ExportMaxBytes        int `env:"DICTIONARY_EXPORT_MAX_BYTES" envDefault:"3145728"`
	ExportMaxBytesCeiling int `env:"DICTIONARY_EXPORT_MAX_BYTES_CEILING" envDefault:"10485760"`
	// ExportPartMaxBytes is the default of the system setting "export part size": the largest part of an export
	// job. A part is streamed to the browser, so only the memory of the worker bounds it (about 2-3 times the size,
	// for up to two jobs per instance); ExportPartMaxBytesCeiling is the most an administrator can set.
	ExportPartMaxBytes        int `env:"DICTIONARY_EXPORT_PART_MAX_BYTES" envDefault:"3145728"`
	ExportPartMaxBytesCeiling int `env:"DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING" envDefault:"52428800"`
	// ExportJobBatchMax is the most words a job reads and packs at once. A job starts with a smaller batch and
	// sizes the next ones from the bytes per word of the last part, to fill most of the size limit.
	ExportJobBatchMax int `env:"DICTIONARY_EXPORT_JOB_BATCH_MAX" envDefault:"20000"`
	// ExportJobTTL is how long the parts of a finished export job can be downloaded; after it the job and its
	// files are deleted.
	ExportJobTTL time.Duration `env:"DICTIONARY_EXPORT_JOB_TTL" envDefault:"24h"`
	// ExportedRetention is the default of the system setting "exported retention": how long words that left the
	// list are kept before the cleanup deletes them (1 hour to 8760 hours).
	ExportedRetention time.Duration `env:"DICTIONARY_EXPORTED_RETENTION" envDefault:"168h"`
	// CleanupInterval is the period of that cleanup.
	CleanupInterval time.Duration `env:"DICTIONARY_CLEANUP_INTERVAL" envDefault:"1h"`

	// HTTPTimeout is the timeout of one call to a dictionary provider and of the download of the audio of a deck.
	HTTPTimeout time.Duration `env:"DICTIONARY_HTTP_TIMEOUT" envDefault:"10s"`

	// FilesDir is the volume the files of the cards are written to: the images in <dir>/images and the audio in
	// <dir>/audio (each <user>/<id>.<ext>). Without it uploads fail with IMAGES_DISABLED / AUDIO_DISABLED.
	// Every image is shrunk to ImageMaxDim and encoded again (jpeg, or png when it has transparency);
	// ImageMaxBytes is the largest result and ImageMaxPixels the largest width x height that is decoded at all.
	// An audio file is kept as it is: AudioMaxBytes is the largest one (the gateway body limit is 1 MiB with base64).
	FilesDir       string `env:"DICTIONARY_FILES_DIR"`
	AudioMaxBytes  int    `env:"DICTIONARY_AUDIO_MAX_BYTES" envDefault:"409600"`
	ImageMaxBytes  int    `env:"DICTIONARY_IMAGE_MAX_BYTES" envDefault:"307200"`
	ImageMaxDim    int    `env:"DICTIONARY_IMAGE_MAX_DIM" envDefault:"1024"`
	ImageMaxPixels int    `env:"DICTIONARY_IMAGE_MAX_PIXELS" envDefault:"25000000"`

	// An image or audio file no card refers to is deleted after UnusedFileAge (an upload comes before the save of the card
	// that uses it). Every OrphanSweepEvery the volume is compared with the table to delete the files without a
	// row (left by a deleted user or a failed delete); OrphanFileAge protects the files of an upload in progress.
	UnusedFileAge    time.Duration `env:"DICTIONARY_UNUSED_FILE_AGE" envDefault:"24h"`
	OrphanSweepEvery time.Duration `env:"DICTIONARY_ORPHAN_SWEEP_EVERY" envDefault:"24h"`
	OrphanFileAge    time.Duration `env:"DICTIONARY_ORPHAN_FILE_AGE" envDefault:"1h"`
	// PurgeGrace is how long the row of a deleted account stays after its data was purged (the purge itself starts
	// at once); there is no undo, the wait only lets the purge finish safely.
	PurgeGrace time.Duration `env:"DICTIONARY_PURGE_GRACE" envDefault:"720h"`

	// AudioHosts are the hosts a saved audio URL may point to: the service downloads them when it builds
	// a deck, so an open list would let a caller make it fetch any address. One entry per line or comma.
	AudioHosts []string `env:"DICTIONARY_AUDIO_HOSTS" envSeparator:"," envDefault:"api.dictionaryapi.dev,upload.wikimedia.org,ssl.gstatic.com,dictionary.cambridge.org"`
}

// Hosts returns the allowed audio hosts, lower case.
func (c Storage) Hosts() []string {
	var out []string
	for _, h := range config.Fields(c.AudioHosts) {
		out = append(out, strings.ToLower(h))
	}
	return out
}

// Worker is the configuration of dictionary-worker: the export jobs and the cleanup. It has no gRPC server and
// verifies no token, so it needs neither the issuer nor the JWKS URL. Variables as in Dictionary, with the
// prefix DICTIONARY_ except the shared blocks of gocommon (LOG_*, POSTGRES_*) and the ones below.
type Worker struct {
	config.Log
	config.Postgres
	Storage

	// HealthAddr is where /healthz and /readyz listen (the probes of Kubernetes).
	HealthAddr      string        `env:"HEALTH_ADDR" envDefault:":8081"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
}

// Validate rejects settings that would start the worker unsafely or uselessly.
func (c Worker) Validate() error { return c.Storage.validate() }

// Dictionary is the configuration of the gRPC server (the API). Every variable has the prefix DICTIONARY_ except
// the shared blocks of gocommon (HOST, PORT, LOG_*, POSTGRES_*). The export jobs and the cleanup run in
// dictionary-worker (Worker).
type Dictionary struct {
	config.Log
	config.Postgres
	// GRPCTLS: the certificate dictionary serves gRPC with and the CA it trusts when it dials identity securely.
	config.GRPCTLS
	Storage

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
	// PermissionsSyncTimeout is how long the start retries to register the permissions (permissions.proto) with
	// identity, which needs IdentityAddr and a client with the role service_registrar. When it runs out the service
	// exits non-zero; until the registration succeeded it is not ready, so a rolling deploy waits on the new pods.
	PermissionsSyncTimeout time.Duration `env:"PERMISSIONS_SYNC_TIMEOUT" envDefault:"2m"`

	// ExportSyncMax is the default of the system setting "export sync max words" (an administrator changes it in the
	// application; the value stored there wins): the most words a synchronous export (ExportAnki, CreateDeckLink)
	// packs; a longer list is refused with EXPORT_USE_JOB. It bounds the memory of one request.
	ExportSyncMax int `env:"DICTIONARY_EXPORT_SYNC_MAX" envDefault:"2000"`
	// DeckLinkTTL is how long a download link of a deck works (it works once).
	DeckLinkTTL time.Duration `env:"DICTIONARY_DECK_LINK_TTL" envDefault:"15m"`
	// ImageUserQuota and AudioUserQuota are the defaults of the system settings "image user quota" and "audio user
	// quota" (an administrator changes them in the application; the value stored there wins): the totals a user
	// may keep (checked on upload, so by the API only). The ceilings are the most an administrator can set, for
	// these two and for the content: ContentMaxBytesCeiling is the largest content of one word (it has to stay
	// under the body limit of the gateway and the message limit of gRPC), ContentUserQuota the default of the total
	// content of the words of a user (checked on save, so by the API only).
	ImageUserQuota          int64 `env:"DICTIONARY_IMAGE_USER_QUOTA" envDefault:"20971520"`
	AudioUserQuota          int64 `env:"DICTIONARY_AUDIO_USER_QUOTA" envDefault:"10485760"`
	ContentUserQuota        int64 `env:"DICTIONARY_CONTENT_USER_QUOTA" envDefault:"268435456"`
	ContentMaxBytesCeiling  int   `env:"DICTIONARY_CONTENT_MAX_BYTES_CEILING" envDefault:"262144"`
	ContentUserQuotaCeiling int64 `env:"DICTIONARY_CONTENT_USER_QUOTA_CEILING" envDefault:"1073741824"`
	ImageUserQuotaCeiling   int64 `env:"DICTIONARY_IMAGE_USER_QUOTA_CEILING" envDefault:"1073741824"`
	AudioUserQuotaCeiling   int64 `env:"DICTIONARY_AUDIO_USER_QUOTA_CEILING" envDefault:"1073741824"`

	// LookupCacheTTL and LookupCacheSize bound the in-memory cache of lookups (the free APIs are not hammered).
	LookupCacheTTL  time.Duration `env:"DICTIONARY_LOOKUP_CACHE_TTL" envDefault:"24h"`
	LookupCacheSize int           `env:"DICTIONARY_LOOKUP_CACHE_SIZE" envDefault:"2000"`
	// UserAgent is sent to the providers (Wikimedia asks for a name and a contact).
	UserAgent string `env:"DICTIONARY_USER_AGENT" envDefault:"huponx-dictionary/1.0 (+https://github.com/huponx/dictionary)"`
	// ViFallbackMT turns on the machine translation (MyMemory) of the English definitions when the
	// Vietnamese Wiktionary has no entry for a word.
	ViFallbackMT bool `env:"DICTIONARY_VI_FALLBACK_MT" envDefault:"true"`

	// Base URLs of the providers (overridden in tests).
	FreeDictURL   string `env:"DICTIONARY_FREEDICT_URL" envDefault:"https://api.dictionaryapi.dev/api/v2/entries/en"`
	WiktionaryURL string `env:"DICTIONARY_WIKTIONARY_URL" envDefault:"https://vi.wiktionary.org/w/api.php"`
	MyMemoryURL   string `env:"DICTIONARY_MYMEMORY_URL" envDefault:"https://api.mymemory.translated.net/get"`
}

// Addr is the gRPC listen address.
func (c Dictionary) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// IsLocal reports a local development run (reflection on, http JWKS allowed).
func (c Dictionary) IsLocal() bool { return strings.EqualFold(c.Env, "local") }

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
	if c.ExportSyncMax < 1 || c.ExportSyncMax > 5000 {
		return fmt.Errorf("DICTIONARY_EXPORT_SYNC_MAX must be between 1 and 5000")
	}
	if c.DeckLinkTTL < time.Minute || c.DeckLinkTTL > 24*time.Hour {
		return fmt.Errorf("DICTIONARY_DECK_LINK_TTL must be between 1m and 24h")
	}
	if c.LookupCacheTTL < 0 || c.LookupCacheSize < 0 {
		return fmt.Errorf("the lookup cache settings must not be negative")
	}
	if c.ImageUserQuota < int64(c.ImageMaxBytes) {
		return fmt.Errorf("DICTIONARY_IMAGE_USER_QUOTA must be at least DICTIONARY_IMAGE_MAX_BYTES")
	}
	if c.AudioUserQuota < int64(c.AudioMaxBytes) {
		return fmt.Errorf("DICTIONARY_AUDIO_USER_QUOTA must be at least DICTIONARY_AUDIO_MAX_BYTES")
	}
	for _, q := range []struct {
		env            string
		quota, ceiling int64
	}{
		{"CONTENT", c.ContentUserQuota, c.ContentUserQuotaCeiling},
		{"IMAGE", c.ImageUserQuota, c.ImageUserQuotaCeiling},
		{"AUDIO", c.AudioUserQuota, c.AudioUserQuotaCeiling},
	} {
		if q.quota < settings.MinUserQuota || q.ceiling < q.quota {
			return fmt.Errorf("DICTIONARY_%s_USER_QUOTA must be at least %d and at most DICTIONARY_%s_USER_QUOTA_CEILING", q.env, settings.MinUserQuota, q.env)
		}
	}
	if c.ContentMaxBytesCeiling < settings.DefaultMaxContentBytes || c.ContentMaxBytesCeiling > 1<<19 {
		return fmt.Errorf("DICTIONARY_CONTENT_MAX_BYTES_CEILING must be between %d and 524288 (under the 1 MiB body limit of the gateway and message limit of gRPC)", settings.DefaultMaxContentBytes)
	}
	return c.Storage.validate()
}

// validate checks what the API and the worker share.
func (c Storage) validate() error {
	if c.MaxSavedWords < 1 || c.MaxSavedWords > settings.MaxSavedWordsLimit {
		return fmt.Errorf("DICTIONARY_MAX_SAVED_WORDS must be between 1 and %d", settings.MaxSavedWordsLimit)
	}
	if c.ExportMaxBytes < 1<<16 {
		return fmt.Errorf("DICTIONARY_EXPORT_MAX_BYTES must be at least 65536")
	}
	if c.ExportMaxBytesCeiling < c.ExportMaxBytes {
		return fmt.Errorf("DICTIONARY_EXPORT_MAX_BYTES_CEILING must be at least DICTIONARY_EXPORT_MAX_BYTES")
	}
	if c.ExportPartMaxBytes < 1<<16 {
		return fmt.Errorf("DICTIONARY_EXPORT_PART_MAX_BYTES must be at least 65536")
	}
	if c.ExportPartMaxBytesCeiling < c.ExportPartMaxBytes {
		return fmt.Errorf("DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING must be at least DICTIONARY_EXPORT_PART_MAX_BYTES")
	}
	if c.ExportJobBatchMax < 100 || c.ExportJobBatchMax > 100_000 {
		return fmt.Errorf("DICTIONARY_EXPORT_JOB_BATCH_MAX must be between 100 and 100000")
	}
	if c.ExportJobTTL < time.Hour || c.ExportJobTTL > 168*time.Hour {
		return fmt.Errorf("DICTIONARY_EXPORT_JOB_TTL must be between 1h and 168h")
	}
	if c.ExportedRetention < time.Hour || c.ExportedRetention > 8760*time.Hour {
		return fmt.Errorf("DICTIONARY_EXPORTED_RETENTION must be between 1h and 8760h")
	}
	if c.CleanupInterval < time.Minute {
		return fmt.Errorf("DICTIONARY_CLEANUP_INTERVAL must be at least 1m")
	}
	if c.ImageMaxBytes < 1<<10 || c.ImageMaxBytes > 700<<10 {
		return fmt.Errorf("DICTIONARY_IMAGE_MAX_BYTES must be between 1024 and 716800 (the gateway body limit is 1 MiB with base64)")
	}
	if c.ImageMaxDim < 128 || c.ImageMaxDim > 4096 {
		return fmt.Errorf("DICTIONARY_IMAGE_MAX_DIM must be between 128 and 4096")
	}
	if c.ImageMaxPixels < 1<<16 || c.ImageMaxPixels > 100_000_000 {
		return fmt.Errorf("DICTIONARY_IMAGE_MAX_PIXELS must be between 65536 and 100000000")
	}
	if c.AudioMaxBytes < 1<<10 || c.AudioMaxBytes > 700<<10 {
		return fmt.Errorf("DICTIONARY_AUDIO_MAX_BYTES must be between 1024 and 716800 (the gateway body limit is 1 MiB with base64)")
	}
	if c.UnusedFileAge < time.Hour {
		return fmt.Errorf("DICTIONARY_UNUSED_FILE_AGE must be at least 1h (an upload comes before the save of its card)")
	}
	if c.OrphanSweepEvery < time.Minute {
		return fmt.Errorf("DICTIONARY_ORPHAN_SWEEP_EVERY must be at least 1m")
	}
	if c.OrphanFileAge < 10*time.Minute {
		return fmt.Errorf("DICTIONARY_ORPHAN_FILE_AGE must be at least 10m (it protects an upload in progress)")
	}
	if c.PurgeGrace < 0 {
		return fmt.Errorf("DICTIONARY_PURGE_GRACE must not be negative")
	}
	if len(c.Hosts()) == 0 {
		return fmt.Errorf("DICTIONARY_AUDIO_HOSTS must list at least one host")
	}
	return nil
}
