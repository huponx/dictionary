package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/appconfig"
	"github.com/huponx/dictionary/internal/identityclient"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/service"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcclient"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/jwtx"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/tokenexchange"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// maxMessageMargin is added to the export limit for the rest of a gRPC message.
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
	looker := lookup.New(map[lookup.Dictionary]lookup.Provider{
		lookup.English:           freeDict,
		lookup.EnglishVietnamese: vi,
	}, cfg.LookupCacheTTL, cfg.LookupCacheSize)

	decks := &anki.Builder{
		Client: &http.Client{Timeout: cfg.HTTPTimeout}, AllowedHosts: cfg.Hosts(), MaxBytes: cfg.ExportMaxBytes, Log: log,
	}

	srv := grpcserver.New(
		grpcserver.WithLogger(log),
		grpcserver.WithReflection(cfg.IsLocal()),
		grpcserver.WithJWTAuth(verifier),
		grpcserver.WithMethodPermissions(service.Permissions),
		grpcserver.WithMaxMsgSize(1<<20, cfg.ExportMaxBytes+maxMessageMargin),
	)
	svc := service.New(looker, st, decks, service.Config{
		MaxSavedWords: cfg.MaxSavedWords, AudioHosts: cfg.Hosts(), DeckLinkTTL: cfg.DeckLinkTTL,
	})
	if cfg.IdentityAddr != "" {
		// preferences of the users: identity, called on behalf of the user of the request
		exch, err := tokenexchange.New(tokenexchange.Config{TokenURL: cfg.TokenURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret})
		if err != nil {
			log.Fatal("token exchange", zap.Error(err))
		}
		conn, err := grpcclient.Dial(ctx, cfg.IdentityAddr,
			grpcclient.WithLogger(log), grpcclient.WithInsecure(cfg.IdentityInsecure),
			grpcclient.WithTokenExchange(exch, cfg.IdentityAudience))
		if err != nil {
			log.Fatal("identity client", zap.Error(err))
		}
		defer conn.Close()
		svc.WithPrefs(identityclient.New(conn))
	} else {
		log.Warn("DICTIONARY_IDENTITY_ADDR is not set: the preferences of the users are the defaults and cannot be changed")
	}
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
	go cleanup(ctx, st, cfg, log)

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

// cleanupBatch is the number of rows one DELETE removes.
const cleanupBatch = 500

// cleanup deletes the exported words older than the retention, every CleanupInterval, in batches. Several
// replicas can run it at once.
func cleanup(ctx context.Context, st *store.Store, cfg appconfig.Dictionary, log *zap.Logger) {
	t := time.NewTicker(cfg.CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var total int64
			for {
				n, err := st.DeleteExportedBefore(ctx, time.Now().Add(-cfg.ExportedRetention), cleanupBatch)
				if err != nil {
					log.Warn("cleanup exported words", zap.Error(err))
					break
				}
				total += n
				if n < cleanupBatch {
					break
				}
			}
			if total > 0 {
				log.Info("exported words deleted", zap.Int64("count", total))
			}
			if n, err := st.DeleteExpiredDeckLinks(ctx, time.Now()); err != nil {
				log.Warn("cleanup deck links", zap.Error(err))
			} else if n > 0 {
				log.Info("expired deck links deleted", zap.Int64("count", n))
			}
		}
	}
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "dictionary"
	}
	return name
}
