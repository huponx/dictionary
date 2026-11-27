package service

import (
	"context"
	"time"

	"github.com/huponx/dictionary/internal/srs"
	"github.com/huponx/dictionary/internal/store"
)

// The fake keeps the words of the group "en" itself and the words of every other group in a fake of its own, which
// shares the categories of the user: the same word in two groups never collides, like the key (user, group, word) of
// the real store. The methods below pick the fake of the group of the call; the ones in the other test files
// (lower case) hold the logic of one group.

// in returns the fake that holds the words of the group.
func (f *fakeStore) in(group string) *fakeStore {
	if group == "" || group == "en" {
		return f
	}
	if f.groups == nil {
		f.groups = map[string]*fakeStore{}
	}
	g, ok := f.groups[group]
	if !ok {
		g = newFakeStore()
		g.clock = f.clock
		f.groups[group] = g
	}
	g.cats, g.settings, g.images, g.audio = f.cats, f.settings, f.images, f.audio
	return g
}

func (f *fakeStore) SaveWord(ctx context.Context, user string, w store.SavedWord, max int, contentQuota int64, categoryIDs []string) (int, error) {
	return f.in(w.Group).saveWord(ctx, user, w, max, contentQuota, categoryIDs)
}
func (f *fakeStore) CountPending(ctx context.Context, user string, flt store.WordFilter) (int, int, bool, error) {
	return f.in(flt.Group).countPending(ctx, user, flt)
}
func (f *fakeStore) ListPending(ctx context.Context, user string, after time.Time, afterWord, query string, flt store.WordFilter, limit int) ([]store.SavedWord, error) {
	return f.in(flt.Group).listPending(ctx, user, after, afterWord, query, flt, limit)
}
func (f *fakeStore) Pending(ctx context.Context, user string, scope store.Scope, flt store.WordFilter, words []string, limit int) ([]store.SavedWord, error) {
	return f.in(flt.Group).pending(ctx, user, scope, flt, words, limit)
}
func (f *fakeStore) PendingPage(ctx context.Context, user string, scope store.Scope, flt store.WordFilter, afterCreated time.Time, afterWord string, limit int) ([]store.SavedWord, error) {
	return f.in(flt.Group).pendingPage(ctx, user, scope, flt, afterCreated, afterWord, limit)
}
func (f *fakeStore) CountExportable(ctx context.Context, user string, scope store.Scope, flt store.WordFilter) (int, error) {
	return f.in(flt.Group).countExportable(ctx, user, scope, flt)
}
func (f *fakeStore) ListCategories(ctx context.Context, user, group string) (store.Categories, error) {
	return f.in(group).listCategories(ctx, user)
}
func (f *fakeStore) SetWordCategories(ctx context.Context, user, group string, words, add, remove []string) (int, error) {
	return f.in(group).setWordCategories(ctx, user, words, add, remove)
}
func (f *fakeStore) DeletePending(ctx context.Context, user, group, word string) error {
	return f.in(group).deletePending(ctx, user, word)
}
func (f *fakeStore) SavedByNames(ctx context.Context, user, group string, words []string) (map[string][]string, error) {
	return f.in(group).savedByNames(ctx, user, words)
}
func (f *fakeStore) MarkExported(ctx context.Context, user, group string, words []string, remove bool) (int64, error) {
	return f.in(group).markExported(ctx, user, words, remove)
}
func (f *fakeStore) RemoveExported(ctx context.Context, user, group string) (int64, error) {
	return f.in(group).removeExported(ctx, user)
}
func (f *fakeStore) ReviewQueue(ctx context.Context, user string, now time.Time, flt store.WordFilter, limit, dueAllowed, newAllowed int) ([]store.ReviewCard, error) {
	return f.in(flt.Group).reviewQueue(ctx, user, now, flt, limit, dueAllowed, newAllowed)
}
func (f *fakeStore) ReviewCounts(ctx context.Context, user string, now time.Time, flt store.WordFilter) (store.ReviewCounts, error) {
	return f.in(flt.Group).reviewCounts(ctx, user, now, flt)
}
func (f *fakeStore) ReviewToday(ctx context.Context, user, group string, now time.Time) (int, int, error) {
	return f.in(group).reviewToday(ctx, user, now)
}
func (f *fakeStore) PutReview(ctx context.Context, user, group, word string, now time.Time, g srs.Grade, next func(srs.State) srs.State) (srs.State, error) {
	return f.in(group).putReview(ctx, user, word, now, g, next)
}
