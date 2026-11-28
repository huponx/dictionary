package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/appconfig"
	"github.com/huponx/dictionary/internal/catalog"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/identityclient"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/service"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcclient"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/jwtx"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/permsync"
	"github.com/huponx/gocommon/tlsconfig"
	"github.com/huponx/gocommon/tokenexchange"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	identityv1 "github.com/huponx/protogen/go/identity/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// maxMessageMargin is added to the ceiling of the export size for the rest of a gRPC message.
const maxMessageMargin = 64 << 10

func main() {
	cfg := config.MustLoad[appconfig.Dictionary]()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	log, err := logging.New(logging.Config{Level: cfg.Level, Encoding: cfg.Encoding, Service: serviceName(cfg.Service), Env: cfg.Env})
	if err != nil {
		panic(err)
	}
	defer logging.Sync(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.Postgres.URL())
	if err != nil {
		log.Fatal("postgres", zap.Error(err))
	}
	defer st.Close()

	// gRPC API: the JWTs identity issues for this service (aud dictionary), exchanged by gatekeeper
	keys, err := jwtx.NewRemoteKeySet(jwtx.RemoteKeySetConfig{URL: cfg.JWKSURL})
	if err != nil {
		log.Fatal("jwks", zap.Error(err))
	}
	verifier, err := jwtx.NewVerifier(jwtx.VerifierConfig{Issuer: cfg.Issuer, Audience: cfg.AuthAudience, Keys: keys})
	if err != nil {
		log.Fatal("jwt verifier", zap.Error(err))
	}

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	freeDict := lookup.NewFreeDict(cfg.FreeDictURL, httpClient, cfg.UserAgent)
	vi := &lookup.Vietnamese{
		English:    freeDict,
		Wiktionary: lookup.NewViWiktionary(cfg.WiktionaryURL, httpClient, cfg.UserAgent),
	}
	if cfg.ViFallbackMT {
		vi.Translator = lookup.NewMyMemory(cfg.MyMemoryURL, httpClient, cfg.UserAgent)
	}
	// The dictionaries are listed in the catalog package; a provider per id goes here.
	registry := catalog.Registry(map[string]lookup.Provider{catalog.English: freeDict, catalog.EnglishVietnamese: vi})
	looker := lookup.New(registry, cfg.LookupCacheTTL, cfg.LookupCacheSize)

	decks := &anki.Builder{
		Client: &http.Client{Timeout: cfg.HTTPTimeout}, AllowedHosts: cfg.Hosts(), MaxBytes: cfg.ExportMaxBytesCeiling, Log: log,
	}

	grpcOpts := []grpcserver.Option{
		grpcserver.WithLogger(log),
		grpcserver.WithReflection(cfg.IsLocal()),
		grpcserver.WithJWTAuth(verifier),
		grpcserver.WithMethodPermissions(service.Permissions),
		grpcserver.WithMaxMsgSize(1<<20, cfg.ExportMaxBytesCeiling+maxMessageMargin),
	}
	if st := cfg.ServerTLS(); st.Enabled() {
		tc, err := tlsconfig.Server(st)
		if err != nil {
			log.Fatal("grpc tls", zap.Error(err))
		}
		grpcOpts = append(grpcOpts, grpcserver.WithTLS(tc))
		log.Info("grpc serves TLS", zap.String("cert", st.CertFile))
	}
	srv := grpcserver.New(grpcOpts...)
	svc := service.New(looker, registry, st, decks, service.Config{
		MaxSavedWords: cfg.MaxSavedWords, ExportedRetentionHours: int(cfg.ExportedRetention / time.Hour), AudioHosts: cfg.Hosts(), DeckLinkTTL: cfg.DeckLinkTTL,
		ExportSyncMax: cfg.ExportSyncMax, ExportJobTTL: cfg.ExportJobTTL,
		ExportMaxBytes: cfg.ExportMaxBytes, ExportPartMaxBytes: cfg.ExportPartMaxBytes,
		ExportMaxBytesCeiling: cfg.ExportMaxBytesCeiling, ExportPartMaxBytesCeiling: cfg.ExportPartMaxBytesCeiling,
		ExportJobBatchMax: cfg.ExportJobBatchMax,
		Storage: settings.StorageLimits{
			MaxContentBytesCeiling: cfg.ContentMaxBytesCeiling,
			ContentUserQuota:       cfg.ContentUserQuota, ContentUserQuotaCeiling: cfg.ContentUserQuotaCeiling,
			ImageUserQuotaCeiling: cfg.ImageUserQuotaCeiling, AudioUserQuotaCeiling: cfg.AudioUserQuotaCeiling,
		},
	})
	// the export jobs and the cleanup run in dictionary-worker, which shares this volume
	if cfg.FilesDir != "" {
		svc.WithImages(&filestore.Store{Dir: filepath.Join(cfg.FilesDir, "images")}, filestore.ImageOptions{MaxBytes: cfg.ImageMaxBytes, MaxDim: cfg.ImageMaxDim, MaxPixels: cfg.ImageMaxPixels}, cfg.ImageUserQuota)
		svc.WithAudio(&filestore.Store{Dir: filepath.Join(cfg.FilesDir, "audio")}, cfg.AudioMaxBytes, cfg.AudioUserQuota)
		// the deck files of the export jobs (parts of a long list)
		svc.WithExports(&filestore.Store{Dir: filepath.Join(cfg.FilesDir, "exports")})
	} else {
		log.Warn("DICTIONARY_FILES_DIR is not set: the images and the audio files of the cards are disabled")
	}
	var registrar permsync.Registrar
	if cfg.IdentityAddr != "" {
		// preferences of the users: identity, called on behalf of the user of the request
		exch, err := tokenexchange.New(tokenexchange.Config{TokenURL: cfg.TokenURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret})
		if err != nil {
			log.Fatal("token exchange", zap.Error(err))
		}
		conn, err := grpcclient.Dial(ctx, cfg.IdentityAddr, append(identityTLS(cfg),
			grpcclient.WithLogger(log), grpcclient.WithInsecure(cfg.IdentityInsecure),
			grpcclient.WithTokenExchange(exch, cfg.IdentityAudience))...)
		if err != nil {
			log.Fatal("identity client", zap.Error(err))
		}
		defer conn.Close()
		// the permissions of this service: identity, called as the service itself
		regConn, err := grpcclient.Dial(ctx, cfg.IdentityAddr, append(identityTLS(cfg),
			grpcclient.WithLogger(log), grpcclient.WithInsecure(cfg.IdentityInsecure),
			grpcclient.WithServiceToken(exch, cfg.IdentityAudience))...)
		if err != nil {
			log.Fatal("identity client", zap.Error(err))
		}
		defer regConn.Close()
		registrar = identityv1.NewIdentityServiceClient(regConn)
		svc.WithPrefs(identityclient.New(conn))
		svc.WithScheduleCache(scheduleCacheTTL, scheduleCacheUsers)
	} else {
		log.Warn("DICTIONARY_IDENTITY_ADDR is not set: the preferences of the users are the defaults and cannot be changed")
	}
	svc.WithCategoryCache(categoryCacheTTL, categoryCacheUsers)
	dictionaryv1.RegisterDictionaryServiceServer(srv.GRPC, svc)

	lis, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatal("listen", zap.Error(err), zap.String("addr", cfg.Addr()))
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("dictionary grpc listening", zap.String("addr", cfg.Addr()))
		errCh <- srv.GRPC.Serve(lis)
	}()

	// Not ready until identity has the permissions of this service: a changed set changes what many users may do. The
	// port is open (so the probe gets an answer) but reports NOT_SERVING; after the timeout the process exits non-zero.
	if registrar != nil {
		srv.SetServing("", false)
		if err := permsync.RegisterWithRetry(ctx, log, registrar, "dictionary", cfg.PermissionsSyncTimeout,
			dictionaryv1.File_dictionary_v1_permissions_proto); err != nil {
			if ctx.Err() == nil {
				log.Error("permission sync", zap.Error(err))
				os.Exit(1)
			}
		}
		srv.SetServing("", true)
	} else {
		log.Warn("DICTIONARY_IDENTITY_ADDR is not set: the permissions of the dictionary are not registered with identity")
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error("serve", zap.Error(err))
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	srv.Stop(shutCtx)
}

// The schedule settings of a user (preferences and FSRS weights) stay in memory this long, for this many users.
const (
	scheduleCacheTTL   = time.Minute
	scheduleCacheUsers = 50000
	// the category counts of a user stay in memory this long, for this many users.
	categoryCacheTTL   = 30 * time.Second
	categoryCacheUsers = 10000
)

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "dictionary"
	}
	return name
}

// identityTLS is the TLS option of the identity dials, none while DICTIONARY_IDENTITY_INSECURE is true.
func identityTLS(cfg appconfig.Dictionary) []grpcclient.Option {
	if cfg.IdentityInsecure {
		return nil
	}
	return []grpcclient.Option{grpcclient.WithTLSFiles(cfg.ClientTLS())}
}
