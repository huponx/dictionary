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
	"github.com/huponx/dictionary/internal/imagestore"
	"github.com/huponx/dictionary/internal/lookup"
	"github.com/huponx/dictionary/internal/service"
	"github.com/huponx/dictionary/internal/settings"
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
		MaxSavedWords: cfg.MaxSavedWords, ExportedRetentionHours: int(cfg.ExportedRetention / time.Hour), AudioHosts: cfg.Hosts(), DeckLinkTTL: cfg.DeckLinkTTL,
	})
	var files *imagestore.Store
	if cfg.ImageDir != "" {
		files = &imagestore.Store{Dir: cfg.ImageDir}
		svc.WithImages(files, imagestore.Options{MaxBytes: cfg.ImageMaxBytes, MaxDim: cfg.ImageMaxDim, MaxPixels: cfg.ImageMaxPixels}, cfg.ImageUserQuota)
	} else {
		log.Warn("DICTIONARY_IMAGE_DIR is not set: the images of the cards are disabled")
	}
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
	go cleanup(ctx, st, files, cfg, log)

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

// retention is how long a word that left the list is kept: the system setting of the administrators, or the
// default of the configuration (DICTIONARY_EXPORTED_RETENTION) when the settings cannot be read or hold none.
func retention(ctx context.Context, st *store.Store, cfg appconfig.Dictionary, log *zap.Logger) time.Duration {
	def := settings.Defaults(cfg.MaxSavedWords, int(cfg.ExportedRetention/time.Hour))
	rows, err := st.Settings(ctx)
	if err != nil {
		log.Warn("read settings for the cleanup, using the default retention", zap.Error(err))
		return cfg.ExportedRetention
	}
	return time.Duration(settings.FromRows(rows, def).ExportedRetentionHours) * time.Hour
}

// cleanup deletes the words that left the list (an export took them out, or the user chose to remove them)
// longer ago than the retention, every CleanupInterval, in batches. Words still in the list are never
// deleted. Several replicas can run it at once.
func cleanup(ctx context.Context, st *store.Store, files *imagestore.Store, cfg appconfig.Dictionary, log *zap.Logger) {
	t := time.NewTicker(cfg.CleanupInterval)
	defer t.Stop()
	lastSweep := time.Now() // the first sweep of the files is a day after the start
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var total int64
			keep := retention(ctx, st, cfg, log)
			for {
				n, err := st.DeleteRemovedBefore(ctx, time.Now().Add(-keep), cleanupBatch)
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
			if files != nil {
				cleanImages(ctx, st, files, cfg.UnusedImageAge, log)
				if time.Since(lastSweep) >= cfg.OrphanSweepEvery {
					lastSweep = time.Now()
					sweepOrphanFiles(ctx, st, files, cfg.OrphanFileAge, log)
				}
			}
			if n, err := st.DeleteExpiredDeckLinks(ctx, time.Now()); err != nil {
				log.Warn("cleanup deck links", zap.Error(err))
			} else if n > 0 {
				log.Info("expired deck links deleted", zap.Int64("count", n))
			}
		}
	}
}

// cleanImages deletes the images no word uses (their rows first, then their files), in batches.
func cleanImages(ctx context.Context, st *store.Store, files *imagestore.Store, unusedAge time.Duration, log *zap.Logger) {
	var total int
	for {
		rows, err := st.DeleteUnusedImages(ctx, time.Now().Add(-unusedAge), cleanupBatch)
		if err != nil {
			log.Warn("cleanup images", zap.Error(err))
			break
		}
		for _, im := range rows {
			if err := files.Remove(im.UserID, im.ID, imagestore.Ext(im.ContentType)); err != nil {
				// the row is gone: the sweep of orphan files deletes the file later
				log.Warn("delete image file", zap.String("id", im.ID), zap.Error(err))
			}
		}
		total += len(rows)
		if len(rows) < cleanupBatch {
			break
		}
	}
	if total > 0 {
		log.Info("unused images deleted", zap.Int("count", total))
	}
}

// sweepOrphanFiles deletes the files of the volume that have no row.
func sweepOrphanFiles(ctx context.Context, st *store.Store, files *imagestore.Store, minAge time.Duration, log *zap.Logger) {
	users, err := files.Users()
	if err != nil {
		log.Warn("sweep image files", zap.Error(err))
		return
	}
	var total int
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		n, err := files.Orphans(u, minAge, cleanupBatch, func(fs []imagestore.File) (map[string]bool, error) {
			ids := make([]string, len(fs))
			for i, f := range fs {
				ids[i] = f.ID
			}
			return st.ExistingImageIDs(ctx, ids)
		})
		total += n
		if err != nil {
			log.Warn("sweep image files", zap.String("user", u), zap.Error(err))
		}
	}
	if total > 0 {
		log.Info("orphan image files deleted", zap.Int("count", total))
	}
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "dictionary"
	}
	return name
}
