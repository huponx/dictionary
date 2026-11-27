// Package cleanup is the housekeeping of dictionary-worker: it deletes what is no longer needed (words that left
// the list, the old review log, unused or orphan files, expired export jobs and deck links) every interval.
package cleanup

import (
	"context"
	"time"

	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/service"
	"github.com/huponx/dictionary/internal/settings"
	"github.com/huponx/dictionary/internal/store"
	"go.uber.org/zap"
)

// reviewLogKeep is how long the log of the reviews stays (the optimizer reads the latest ones).
const reviewLogKeep = 24 * 30 * 24 * time.Hour

// Config is what the cleanup needs of the configuration.
type Config struct {
	// Interval is the period of the cleanup; UnusedFileAge, OrphanSweepEvery and OrphanFileAge are described
	// in appconfig.Storage.
	Interval         time.Duration
	UnusedFileAge    time.Duration
	OrphanSweepEvery time.Duration
	OrphanFileAge    time.Duration

	// PurgeGrace is how long the row of a purged deleted account stays (see appconfig.PurgeGrace). The rest are the
	// limits of the purge of one tick (users, rows of a batch, batches of a user, time) and take defaults when 0.
	PurgeGrace   time.Duration
	PurgeUsers   int
	PurgeBatch   int
	PurgeBatches int
	PurgeBudget  time.Duration

	// Retention is the default of the retention of the words that left the list, the rest the defaults of the
	// system settings the retention is read from.
	Retention                 time.Duration
	MaxSavedWords             int
	ExportMaxBytes            int
	ExportPartMaxBytes        int
	ExportMaxBytesCeiling     int
	ExportPartMaxBytesCeiling int
}

// Cleaner runs the cleanup. Files and Audio are nil when the volume of the files is not configured.
type Cleaner struct {
	Store        *store.Store
	Service      *service.Service
	Files, Audio *filestore.Store
	// Exports is the volume of the export parts (the purge deletes the directory of a deleted user in the three).
	Exports *filestore.Store
	Config  Config
	Log     *zap.Logger
}

// cleanupBatch is the number of rows one DELETE removes.
const cleanupBatch = 500

// retention is how long a word that left the list is kept: the system setting of the administrators, or the
// default of the configuration (DICTIONARY_EXPORTED_RETENTION) when the settings cannot be read or hold none.
func retention(ctx context.Context, st *store.Store, cfg Config, log *zap.Logger) time.Duration {
	def := settings.Defaults(cfg.MaxSavedWords, int(cfg.Retention/time.Hour)).
		WithExportLimits(cfg.ExportMaxBytes, cfg.ExportPartMaxBytes, cfg.ExportMaxBytesCeiling, cfg.ExportPartMaxBytesCeiling)
	rows, err := st.Settings(ctx)
	if err != nil {
		log.Warn("read settings for the cleanup, using the default retention", zap.Error(err))
		return cfg.Retention
	}
	return time.Duration(settings.FromRows(rows, def).ExportedRetentionHours) * time.Hour
}

// reconcileBatch is the number of users whose counters are recomputed per cleanup tick.
const reconcileBatch = 50

// reconcileCounters recomputes the counters of one batch of users and logs the ones that were wrong. It returns the
// cursor for the next call.
func reconcileCounters(ctx context.Context, st *store.Store, after string, log *zap.Logger) string {
	next, fixed, err := st.ReconcileCounters(ctx, after, reconcileBatch)
	if err != nil {
		log.Warn("reconcile listed counters", zap.Error(err))
		return after
	}
	for u, was := range fixed {
		log.Warn("counter was wrong and is fixed", zap.String("user", u),
			zap.Int64("listed was", was.Listed), zap.Int64("content bytes was", was.ContentBytes),
			zap.Int64("image bytes was", was.ImageBytes), zap.Int64("audio bytes was", was.AudioBytes))
	}
	return next
}

// Run deletes the words that left the list (an export took them out, or the user chose to remove them)
// longer ago than the retention, every Interval, in batches, until ctx ends. Words still in the list are never
// deleted. Several replicas can run it at once.
func (c *Cleaner) Run(ctx context.Context) {
	st, svc, files, audio, cfg, log := c.Store, c.Service, c.Files, c.Audio, c.Config, c.Log
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	var reconcileAfter string // cursor of the counter check, one batch of users per tick
	lastSweep := time.Now()   // the first sweep of the files is a day after the start
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
			c.PurgeDeleted(ctx)
			cleanReviewLog(ctx, st, log)
			reconcileAfter = reconcileCounters(ctx, st, reconcileAfter, log)
			sweepDue := time.Since(lastSweep) >= cfg.OrphanSweepEvery
			if sweepDue {
				lastSweep = time.Now()
			}
			if files != nil {
				cleanImages(ctx, st, files, cfg.UnusedFileAge, log)
				cleanAudio(ctx, st, audio, cfg.UnusedFileAge, log)
				if sweepDue {
					sweepOrphanFiles(ctx, "image", files, st.ExistingImageIDs, cfg.OrphanFileAge, log)
					sweepOrphanFiles(ctx, "audio", audio, st.ExistingAudioIDs, cfg.OrphanFileAge, log)
				}
			}
			if sweepDue {
				// the parts of the export jobs: a file written by a worker that died before it recorded the part
				if n, err := svc.SweepExportFiles(ctx, cfg.OrphanFileAge, cleanupBatch); err != nil {
					log.Warn("sweep export files", zap.Error(err))
				} else if n > 0 {
					log.Info("orphan export files deleted", zap.Int("count", n))
				}
			}
			if n, err := svc.CleanExports(ctx, cleanupBatch); err != nil {
				log.Warn("cleanup export jobs", zap.Error(err))
			} else if n > 0 {
				log.Info("expired export jobs deleted", zap.Int("count", n))
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
func cleanImages(ctx context.Context, st *store.Store, files *filestore.Store, unusedAge time.Duration, log *zap.Logger) {
	var total int
	for {
		rows, err := st.DeleteUnusedImages(ctx, time.Now().Add(-unusedAge), cleanupBatch)
		if err != nil {
			log.Warn("cleanup images", zap.Error(err))
			break
		}
		for _, im := range rows {
			if err := files.Remove(im.UserID, im.ID, filestore.Ext(im.ContentType)); err != nil {
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

// cleanAudio deletes the audio files no word uses (their rows first, then their files), in batches.
func cleanAudio(ctx context.Context, st *store.Store, files *filestore.Store, unusedAge time.Duration, log *zap.Logger) {
	var total int
	for {
		rows, err := st.DeleteUnusedAudio(ctx, time.Now().Add(-unusedAge), cleanupBatch)
		if err != nil {
			log.Warn("cleanup audio", zap.Error(err))
			break
		}
		for _, a := range rows {
			if err := files.Remove(a.UserID, a.ID, filestore.Ext(a.ContentType)); err != nil {
				// the row is gone: the sweep of orphan files deletes the file later
				log.Warn("delete audio file", zap.String("id", a.ID), zap.Error(err))
			}
		}
		total += len(rows)
		if len(rows) < cleanupBatch {
			break
		}
	}
	if total > 0 {
		log.Info("unused audio deleted", zap.Int("count", total))
	}
}

// sweepOrphanFiles deletes the files of the volume (kind: "image" or "audio") that have no row; existing tells
// which of some ids have one.
func sweepOrphanFiles(ctx context.Context, kind string, files *filestore.Store, existing func(context.Context, []string) (map[string]bool, error), minAge time.Duration, log *zap.Logger) {
	users, err := files.Users()
	if err != nil {
		log.Warn("sweep "+kind+" files", zap.Error(err))
		return
	}
	var total int
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		n, err := files.Orphans(u, minAge, cleanupBatch, func(fs []filestore.File) (map[string]bool, error) {
			ids := make([]string, len(fs))
			for i, f := range fs {
				ids[i] = f.ID
			}
			return existing(ctx, ids)
		})
		total += n
		if err != nil {
			log.Warn("sweep "+kind+" files", zap.String("user", u), zap.Error(err))
		}
	}
	if total > 0 {
		log.Info("orphan "+kind+" files deleted", zap.Int("count", total))
	}
}

// cleanReviewLog deletes the review log older than reviewLogKeep, in batches (the log is append-only in time).
func cleanReviewLog(ctx context.Context, st *store.Store, log *zap.Logger) {
	var total int64
	for {
		n, err := st.DeleteReviewLogsBefore(ctx, time.Now().Add(-reviewLogKeep), 5000)
		if err != nil {
			log.Warn("cleanup review log", zap.Error(err))
			break
		}
		total += n
		if n < 5000 {
			break
		}
	}
	if total > 0 {
		log.Info("review log rows deleted", zap.Int64("count", total))
	}
}
