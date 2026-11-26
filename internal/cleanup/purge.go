package cleanup

import (
	"context"
	"time"

	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/store"
	"go.uber.org/zap"
)

// The purge of deleted accounts (identity DeleteUser). A direct DELETE of the user would cascade over its words,
// review log, files and categories in one statement; each tick instead takes a few deleted users and deletes their
// data in short transactions of one batch each, then the files, and the user row after the grace period.

// Defaults of the limits of one tick (Config leaves them 0).
const (
	purgeUsersPerTick = 5
	purgeBatch        = 5000
	purgeBatchesMax   = 200 // per user and tick: 1,000,000 rows
	purgeBudget       = 30 * time.Second
	// purgeStuckAfter is how long after the delete a user whose data is still not purged is logged as stuck.
	purgeStuckAfter = 24 * time.Hour
)

// PurgeDeleted does the work of one tick for up to PurgeUsers deleted users, within PurgeBudget. Several replicas
// can run it at once: a batch only runs under the lock of the user, a replica that does not get it moves on.
func (c *Cleaner) PurgeDeleted(ctx context.Context) {
	cfg, log := c.Config, c.Log
	users, batch, batches, budget := cfg.PurgeUsers, cfg.PurgeBatch, cfg.PurgeBatches, cfg.PurgeBudget
	if users <= 0 {
		users = purgeUsersPerTick
	}
	if batch <= 0 {
		batch = purgeBatch
	}
	if batches <= 0 {
		batches = purgeBatchesMax
	}
	if budget <= 0 {
		budget = purgeBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	list, err := c.Store.DeletedUsers(ctx, cfg.PurgeGrace, users)
	if err != nil {
		log.Warn("list deleted users", zap.Error(err))
		return
	}
	for _, u := range list {
		if ctx.Err() != nil {
			return
		}
		c.purgeUser(ctx, u, batch, batches)
	}
}

func (c *Cleaner) purgeUser(ctx context.Context, u store.DeletedUser, batch, batches int) {
	st, log := c.Store, c.Log.With(zap.String("user", u.ID))
	start := time.Now()
	if !u.Purged {
		rows := map[string]int64{}
		clean := false
		for i := 0; i < batches && ctx.Err() == nil; i++ {
			table, n, isClean, busy, err := st.PurgeStep(ctx, u.ID, batch)
			if err != nil {
				log.Warn("purge deleted user", zap.Error(err))
				break
			}
			if busy { // another replica (or a save) has the user
				return
			}
			if isClean {
				clean = true
				break
			}
			rows[table] += n
		}
		fields := []zap.Field{zap.Duration("took", time.Since(start))}
		for t, n := range rows {
			fields = append(fields, zap.Int64(t, n))
		}
		if len(rows) > 0 {
			log.Info("deleted user data purged", fields...)
		}
		if !clean {
			if time.Since(u.DeletedAt) > purgeStuckAfter {
				log.Warn("deleted user is not purged yet", zap.Time("deleted at", u.DeletedAt))
			}
			return // the next tick goes on
		}
		// the rows are gone: the files go by directory; an error is only logged, the sweep of orphan files finds the rest
		for _, f := range []*filestore.Store{c.Files, c.Audio, c.Exports} {
			if f == nil {
				continue
			}
			if err := f.RemoveAll(u.ID); err != nil {
				log.Warn("delete files of deleted user", zap.String("dir", f.Dir), zap.Error(err))
			}
		}
		if err := st.MarkPurged(ctx, u.ID); err != nil {
			log.Warn("mark deleted user purged", zap.Error(err))
			return
		}
		log.Info("deleted user purged", zap.Duration("took", time.Since(start)))
		return
	}
	deleted, err := st.FinishUserDelete(ctx, u.ID, c.Config.PurgeGrace)
	if err != nil {
		log.Warn("delete the row of a purged user", zap.Error(err))
		return
	}
	if deleted {
		log.Info("deleted user removed", zap.Time("deleted at", u.DeletedAt))
	}
}
