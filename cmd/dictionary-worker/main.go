// dictionary-worker runs what the API leaves to the background: the export jobs (they build the parts of a long
// list, which takes memory) and the cleanup. Both are safe with several replicas (lease and DELETE in batches).
package main

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/appconfig"
	"github.com/huponx/dictionary/internal/cleanup"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/service"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/healthz"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/worker"
	"go.uber.org/zap"
)

func main() {
	cfg := config.MustLoad[appconfig.Worker]()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	log, err := logging.New(logging.Config{Level: cfg.Level, Encoding: cfg.Encoding, Service: serviceName(cfg.Service), Env: cfg.Env})
	if err != nil {
		panic(err)
	}
	defer logging.Sync(log)

	ctx, stop := worker.Signals()
	defer stop()

	st, err := store.New(ctx, cfg.Postgres.URL())
	if err != nil {
		log.Fatal("postgres", zap.Error(err))
	}
	defer st.Close()

	if cfg.FilesDir == "" {
		// without the volume there is nowhere to write the parts: the jobs stay queued and the cleanup skips the files
		log.Warn("DICTIONARY_FILES_DIR is not set: the export jobs do not run and the files are not cleaned")
	}

	decks := &anki.Builder{
		Client: &http.Client{Timeout: cfg.HTTPTimeout}, AllowedHosts: cfg.Hosts(), MaxBytes: cfg.ExportMaxBytesCeiling, Log: log,
	}
	// The jobs do not look words up, so there is no looker. The limits of the settings are the same as the API's
	// (an export job reads the system settings with the ceilings of this process).
	svc := service.New(nil, st, decks, service.Config{
		MaxSavedWords: cfg.MaxSavedWords, ExportedRetentionHours: int(cfg.ExportedRetention / time.Hour), AudioHosts: cfg.Hosts(),
		ExportJobTTL: cfg.ExportJobTTL, ExportMaxBytes: cfg.ExportMaxBytes, ExportPartMaxBytes: cfg.ExportPartMaxBytes,
		ExportMaxBytesCeiling: cfg.ExportMaxBytesCeiling, ExportPartMaxBytesCeiling: cfg.ExportPartMaxBytesCeiling,
		ExportJobBatchMax: cfg.ExportJobBatchMax,
	})
	// the images and the audio each have a directory: the sweep of orphan files reads a whole one
	var files, audio, exports *filestore.Store
	if cfg.FilesDir != "" {
		files = &filestore.Store{Dir: filepath.Join(cfg.FilesDir, "images")}
		// the quota is checked on upload only (the API)
		svc.WithImages(files, filestore.ImageOptions{MaxBytes: cfg.ImageMaxBytes, MaxDim: cfg.ImageMaxDim, MaxPixels: cfg.ImageMaxPixels}, 0)
		audio = &filestore.Store{Dir: filepath.Join(cfg.FilesDir, "audio")}
		svc.WithAudio(audio, cfg.AudioMaxBytes, 0)
		// the deck files of the export jobs (parts of a long list)
		exports = &filestore.Store{Dir: filepath.Join(cfg.FilesDir, "exports")}
		svc.WithExports(exports)
	}

	// the jobs log through the logger of their context
	wctx := logging.WithCtx(ctx, log)
	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); svc.RunExportWorker(wctx) }()
	cleaner := &cleanup.Cleaner{
		Store: st, Service: svc, Files: files, Audio: audio, Exports: exports, Log: log,
		Config: cleanup.Config{
			Interval: cfg.CleanupInterval, UnusedFileAge: cfg.UnusedFileAge, OrphanSweepEvery: cfg.OrphanSweepEvery, OrphanFileAge: cfg.OrphanFileAge, PurgeGrace: cfg.PurgeGrace,
			Retention: cfg.ExportedRetention, MaxSavedWords: cfg.MaxSavedWords,
			ExportMaxBytes: cfg.ExportMaxBytes, ExportPartMaxBytes: cfg.ExportPartMaxBytes,
			ExportMaxBytesCeiling: cfg.ExportMaxBytesCeiling, ExportPartMaxBytesCeiling: cfg.ExportPartMaxBytesCeiling,
		},
	}
	go func() { defer loops.Done(); cleaner.Run(wctx) }()

	h := worker.NewHealth(cfg.HealthAddr, healthz.Check{Name: "postgres", Fn: st.Ping})
	go func() {
		if err := h.ListenAndServe(); err != nil {
			log.Error("health server", zap.Error(err))
		}
	}()
	log.Info("dictionary worker started", zap.String("health", cfg.HealthAddr))

	<-ctx.Done()
	// A job that is running stops without touching the job: its lease runs out and another replica goes on.
	h.SetReady(false)
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() { loops.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutCtx.Done():
		log.Warn("the loops did not stop in time")
	}
	_ = h.Shutdown(shutCtx)
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "dictionary-worker"
	}
	return name
}
