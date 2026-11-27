package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/store"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// The export jobs of the fake store: one job per user at a time, a lease id the worker has to present.
type fakeExports struct {
	jobs   map[string]*store.ExportJob
	parts  map[string][]store.ExportPart
	tokens map[string]struct {
		job string
		n   int
	} // by hash of the token
	buildingSeen []int // the values written by SetExportBuilding
	shownSeen    []int // built + building after every write: what the page shows
}

func (f *fakeStore) pendingPage(_ context.Context, _ string, scope store.Scope, flt store.WordFilter, afterCreated time.Time, afterWord string, limit int) ([]store.SavedWord, error) {
	var out []store.SavedWord
	for _, r := range f.sorted() {
		if !f.match(r.Word, flt) || (scope != store.ScopeAll && r.Exported) {
			continue
		}
		if afterWord != "" && !r.CreatedAt.After(afterCreated) {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) countExportable(ctx context.Context, user string, scope store.Scope, flt store.WordFilter) (int, error) {
	w, _ := f.PendingPage(ctx, user, scope, flt, time.Time{}, "", 1<<30)
	return len(w), nil
}

func (f *fakeStore) CreateExportJob(_ context.Context, user string, p store.ExportParams, ttl time.Duration) (store.ExportJob, error) {
	for _, j := range f.exp.jobs {
		if j.Status == store.JobQueued || j.Status == store.JobRunning {
			return store.ExportJob{}, store.ErrJobActive
		}
	}
	if f.exp.jobs == nil {
		f.exp.jobs, f.exp.parts, f.exp.tokens = map[string]*store.ExportJob{}, map[string][]store.ExportPart{}, map[string]struct {
			job string
			n   int
		}{}
	}
	j := &store.ExportJob{ID: uuid.NewString(), UserID: user, Status: store.JobQueued, Params: p, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(ttl)}
	f.exp.jobs[j.ID] = j
	return *j, nil
}

func (f *fakeStore) GetExportJob(_ context.Context, _, id string) (store.ExportJob, []store.ExportPart, error) {
	var best *store.ExportJob
	for _, j := range f.exp.jobs {
		if (id == "" || id == "latest" || j.ID == id) && (best == nil || j.CreatedAt.After(best.CreatedAt)) {
			best = j
		}
	}
	if best == nil {
		return store.ExportJob{}, nil, store.ErrNotFound
	}
	return *best, slices.Clone(f.exp.parts[best.ID]), nil
}

func (f *fakeStore) CancelExportJob(_ context.Context, _, id string, _ time.Duration) (store.ExportJob, error) {
	j, ok := f.exp.jobs[id]
	if !ok {
		return store.ExportJob{}, store.ErrNotFound
	}
	if j.Status == store.JobQueued || j.Status == store.JobRunning {
		j.Status, j.LeaseID = store.JobCancelled, ""
	}
	return *j, nil
}

func (f *fakeStore) ClaimExportJob(context.Context, time.Duration) (*store.ExportJob, error) {
	for _, j := range f.exp.jobs {
		if j.Status == store.JobQueued {
			j.Status, j.LeaseID = store.JobRunning, uuid.NewString()
			c := *j
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) owned(id, lease string) (*store.ExportJob, error) {
	j, ok := f.exp.jobs[id]
	if !ok || j.Status != store.JobRunning || j.LeaseID != lease {
		return nil, store.ErrLeaseLost
	}
	return j, nil
}

func (f *fakeStore) RenewExportLease(_ context.Context, id, lease string, _ time.Duration) error {
	_, err := f.owned(id, lease)
	return err
}

func (f *fakeStore) SetExportTotal(_ context.Context, id, lease string, total int) error {
	j, err := f.owned(id, lease)
	if err == nil {
		j.Total = total
	}
	return err
}

func (f *fakeStore) SetExportBuilding(_ context.Context, id, lease string, n int) error {
	if j, err := f.owned(id, lease); err == nil {
		j.Building = n
		f.exp.buildingSeen = append(f.exp.buildingSeen, n)
		f.exp.shownSeen = append(f.exp.shownSeen, j.Built+j.Building)
	}
	return nil
}

func (f *fakeStore) AddExportPart(ctx context.Context, job store.ExportJob, lease string, p store.ExportPart, words []string, after store.SavedWord) error {
	j, err := f.owned(job.ID, lease)
	if err != nil || j.Parts != p.N-1 {
		return store.ErrLeaseLost
	}
	f.exp.parts[j.ID] = append(f.exp.parts[j.ID], p)
	if _, err := f.MarkExported(ctx, job.UserID, job.Params.Group, words, job.Params.Remove); err != nil {
		return err
	}
	j.Parts++
	j.Building = max(j.Building-len(words), 0)
	j.Built += len(words)
	f.exp.shownSeen = append(f.exp.shownSeen, j.Built+j.Building)
	j.Params.AfterCreated, j.Params.AfterWord = after.CreatedAt, after.Word
	return nil
}

func (f *fakeStore) FinishExportJob(_ context.Context, id, lease, status, msg string, ttl time.Duration) error {
	j, err := f.owned(id, lease)
	if err != nil {
		return err
	}
	j.Status, j.Error, j.LeaseID, j.FinishedAt, j.ExpiresAt = status, msg, "", time.Now(), time.Now().Add(ttl)
	return nil
}

func (f *fakeStore) SetExportPartToken(_ context.Context, _, id string, n int, hash []byte) (store.ExportPart, time.Time, error) {
	j, ok := f.exp.jobs[id]
	if !ok || n < 1 || n > len(f.exp.parts[id]) || !j.ExpiresAt.After(time.Now()) {
		return store.ExportPart{}, time.Time{}, store.ErrNotFound
	}
	f.exp.tokens[string(hash)] = struct {
		job string
		n   int
	}{id, n}
	return f.exp.parts[id][n-1], j.ExpiresAt, nil
}

func (f *fakeStore) ExportPartByToken(_ context.Context, hash []byte, now time.Time) (string, store.ExportPart, error) {
	t, ok := f.exp.tokens[string(hash)]
	if !ok || !f.exp.jobs[t.job].ExpiresAt.After(now) {
		return "", store.ExportPart{}, store.ErrNotFound
	}
	return f.exp.jobs[t.job].UserID, f.exp.parts[t.job][t.n-1], nil
}

func (f *fakeStore) DeleteExpiredExportJobs(_ context.Context, now time.Time, _ int) (int, []store.ExpiredFile, error) {
	var files []store.ExpiredFile
	n := 0
	for id, j := range f.exp.jobs {
		if j.ExpiresAt.Before(now) {
			for _, p := range f.exp.parts[id] {
				files = append(files, store.ExpiredFile{UserID: j.UserID, FileID: p.FileID})
			}
			delete(f.exp.jobs, id)
			delete(f.exp.parts, id)
			n++
		}
	}
	return n, files, nil
}

func (f *fakeStore) ExistingExportFileIDs(_ context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, ps := range f.exp.parts {
		for _, p := range ps {
			for _, id := range ids {
				if id == p.FileID {
					out[id] = true
				}
			}
		}
	}
	return out, nil
}

// limitDecks refuses a deck of more than max words, like a deck over the size limit.
type limitDecks struct {
	max   int
	sizes []int
}

func (d *limitDecks) Build(_ context.Context, _ string, w []anki.Word, _ bool) ([]byte, error) {
	if len(w) > d.max {
		return nil, anki.ErrTooLarge
	}
	d.sizes = append(d.sizes, len(w))
	return []byte(fmt.Sprintf("deck of %d", len(w))), nil
}

func jobService(t *testing.T, decks Decks, words int) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	s := New(fakeLooker{}, testRegistry(), st, decks, Config{MaxSavedWords: 100000, ExportedRetentionHours: 168}).WithExports(&filestore.Store{Dir: t.TempDir()})
	for i := range words {
		st.rows[fmt.Sprintf("w%03d", i)] = store.SavedWord{Word: fmt.Sprintf("w%03d", i), Content: "c", CreatedAt: st.clock.Add(time.Duration(i) * time.Second)}
	}
	return s, st
}

func startJob(t *testing.T, s *Service, req *dictionaryv1.StartExportRequest) *dictionaryv1.ExportJob {
	t.Helper()
	r, err := s.StartExport(userCtx(), req)
	if err != nil {
		t.Fatal(err)
	}
	return r.Job
}

// runNext takes the queued job and runs it to the end, like a replica of the worker.
func runNext(t *testing.T, s *Service, st *fakeStore) {
	t.Helper()
	j, err := st.ClaimExportJob(context.Background(), jobLease)
	if err != nil || j == nil {
		t.Fatalf("no job to run: %v", err)
	}
	s.runExportJob(context.Background(), *j)
}

func TestExportJobPacksEveryWordInPartsOnce(t *testing.T) {
	decks := &limitDecks{max: 4}
	s, st := jobService(t, decks, 10)
	job := startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en", DeckName: "Big"})
	if job.Status != store.JobQueued {
		t.Errorf("status = %s", job.Status)
	}
	runNext(t, s, st)
	r, err := s.GetExport(userCtx(), &dictionaryv1.GetExportRequest{Id: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	j := r.Job
	if j.Status != store.JobDone || j.Total != 10 || j.Built != 10 {
		t.Fatalf("job: %+v", j)
	}
	got := 0
	for i, p := range j.Parts {
		if int(p.N) != i+1 || p.WordCount > 4 || p.WordCount == 0 || !strings.HasPrefix(p.Filename, "Big (part ") {
			t.Errorf("part %d: %+v", i, p)
		}
		got += int(p.WordCount)
	}
	if got != 10 || len(j.Parts) < 3 {
		t.Errorf("parts hold %d words in %d parts", got, len(j.Parts))
	}
	// every word was marked exported with its part, and left the list (remove is the default)
	if len(st.rows) != 0 || len(st.exported) != 10 {
		t.Errorf("rows left %d, exported %d", len(st.rows), len(st.exported))
	}
}

func TestExportJobSplitsABatchOverTheSizeLimit(t *testing.T) {
	// 2000 words never fit in one deck of at most 700: the batch is halved until it fits, and the smaller size is kept
	decks := &limitDecks{max: 700}
	s, st := jobService(t, decks, 2500)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, parts, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 2500 {
		t.Fatalf("job: %+v", j)
	}
	for _, p := range parts {
		if p.WordCount > 700 {
			t.Errorf("part of %d words", p.WordCount)
		}
	}
}

func TestExportJobFailsOnACardThatNeverFits(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 0}, 3)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, _, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobFailed || !strings.Contains(j.Error, "too large") {
		t.Errorf("job: %+v", j)
	}
	if len(st.exported) != 0 {
		t.Error("a failed job marked words exported")
	}
}

func TestExportJobOnePerUserAndNothingToExport(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 10}, 3)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	if _, err := s.StartExport(userCtx(), &dictionaryv1.StartExportRequest{Group: "en"}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeExportJobActive) {
		t.Errorf("second job: %v", err)
	}
	runNext(t, s, st)
	if _, err := s.StartExport(userCtx(), &dictionaryv1.StartExportRequest{Group: "en"}); code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), CodeNoWords) {
		t.Errorf("no words left: %v", err)
	}
}

func TestExportJobCancelStopsAndKeepsTheParts(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 2}, 6)
	job := startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	claimed, _ := st.ClaimExportJob(context.Background(), jobLease)
	// the user cancels while the replica works: its next write finds the lease lost and nothing more is marked
	if _, err := s.CancelExport(userCtx(), &dictionaryv1.CancelExportRequest{Id: job.Id}); err != nil {
		t.Fatal(err)
	}
	s.runExportJob(context.Background(), *claimed)
	j, parts, _ := st.GetExportJob(context.Background(), "", job.Id)
	if j.Status != store.JobCancelled || len(parts) != 0 || len(st.exported) != 0 {
		t.Errorf("job %+v, parts %d, exported %d", j, len(parts), len(st.exported))
	}
	// a second cancel is harmless, and a new job can start
	if r, err := s.CancelExport(userCtx(), &dictionaryv1.CancelExportRequest{Id: job.Id}); err != nil || r.Job.Status != store.JobCancelled {
		t.Errorf("second cancel: %v %v", r, err)
	}
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
}

// A replica that lost the lease cannot write a part: the other replica that took the job goes on after the last part
// recorded, with no word packed twice.
func TestExportJobTakenOverContinuesWithoutRepeating(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 2}, 6)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	first, _ := st.ClaimExportJob(context.Background(), jobLease)
	// the first replica packs one part, then dies
	words, _ := st.PendingPage(context.Background(), userUUID, store.ScopeNew, store.WordFilter{}, time.Time{}, "", 2)
	keys := []string{words[0].Word, words[1].Word}
	part := store.ExportPart{N: 1, Filename: "x (part 1).apkg", FileID: uuid.NewString(), WordCount: 2, Size: 1}
	if err := st.AddExportPart(context.Background(), *first, first.LeaseID, part, keys, words[1]); err != nil {
		t.Fatal(err)
	}
	// the lease ends and another replica takes the job (the fake takes a queued one: put it back)
	cur := st.exp.jobs[first.ID]
	cur.Status = store.JobQueued
	second, _ := st.ClaimExportJob(context.Background(), jobLease)
	if err := st.AddExportPart(context.Background(), *first, first.LeaseID, store.ExportPart{N: 2}, nil, words[1]); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("the old replica wrote: %v", err)
	}
	s.runExportJob(context.Background(), *second)
	j, parts, _ := st.GetExportJob(context.Background(), "", first.ID)
	total := 0
	for _, p := range parts {
		total += p.WordCount
	}
	if j.Status != store.JobDone || j.Built != 6 || total != 6 {
		t.Errorf("job %+v, %d words in parts", j, total)
	}
}

func TestExportPartLinkWorksManyTimesUntilExpiry(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 3}, 5)
	job := startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en", DeckName: "D"})
	runNext(t, s, st)
	if _, err := s.CreateExportPartLink(userCtx(), &dictionaryv1.CreateExportPartLinkRequest{Id: job.Id, N: 9}); code(err) != codes.NotFound {
		t.Errorf("unknown part: %v", err)
	}
	link, err := s.CreateExportPartLink(userCtx(), &dictionaryv1.CreateExportPartLinkRequest{Id: job.Id, N: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		name, data, err := download(s, serviceCtx(), link.Token)
		if err != nil || name != "D (part 1).apkg" || !strings.HasPrefix(string(data), "deck of") {
			t.Fatalf("download: %q %q %v", name, data, err)
		}
	}
	if _, _, err := download(s, serviceCtx(), "not-a-token-not-a-token-not-a-token"); code(err) != codes.NotFound {
		t.Errorf("unknown token: %v", err)
	}
	if _, _, err := download(s, userCtx(), link.Token); code(err) != codes.PermissionDenied {
		t.Errorf("not a service token: %v", err)
	}
	// the job expires: the link stops and the cleanup deletes the files
	st.exp.jobs[job.Id].ExpiresAt = time.Now().Add(-time.Minute)
	if _, _, err := download(s, serviceCtx(), link.Token); code(err) != codes.NotFound {
		t.Errorf("expired: %v", err)
	}
	_, parts, _ := st.GetExportJob(context.Background(), "", job.Id)
	file := parts[0].FileID
	if n, err := s.CleanExports(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("CleanExports: %d %v", n, err)
	}
	if _, err := s.exports.Read(userUUID, file, "apkg"); !errors.Is(err, filestore.ErrNotFound) {
		t.Errorf("the file of an expired job is still there: %v", err)
	}
}

func TestSweepExportFilesDeletesWhatNoPartRecords(t *testing.T) {
	ctx := context.Background()
	s, st := jobService(t, &limitDecks{max: 3}, 5)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en", DeckName: "D"})
	runNext(t, s, st)
	var recorded string
	for _, ps := range st.exp.parts {
		recorded = ps[0].FileID
	}
	age := func(id string, by time.Duration) {
		t.Helper()
		name := filepath.Join(s.exports.Dir, userUUID, id+".apkg")
		old := time.Now().Add(-by)
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// a worker that died after it wrote a part and before it recorded it
	orphan, young := uuid.NewString(), uuid.NewString()
	for _, id := range []string{orphan, young} {
		if err := s.exports.Write(userUUID, id, "apkg", []byte("deck")); err != nil {
			t.Fatal(err)
		}
	}
	age(orphan, 2*time.Hour)
	age(recorded, 2*time.Hour)
	n, err := s.SweepExportFiles(ctx, time.Hour, 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if _, err := s.exports.Read(userUUID, orphan, "apkg"); !errors.Is(err, filestore.ErrNotFound) {
		t.Errorf("the orphan is still there: %v", err)
	}
	if _, err := s.exports.Read(userUUID, recorded, "apkg"); err != nil {
		t.Errorf("the file of a recorded part was deleted: %v", err)
	}
	if _, err := s.exports.Read(userUUID, young, "apkg"); err != nil {
		t.Errorf("a young file (a part being written) was deleted: %v", err)
	}
	// without a directory there is nothing to sweep
	if n, err := New(fakeLooker{}, testRegistry(), st, &fakeDecks{}, Config{}).SweepExportFiles(ctx, time.Hour, 10); err != nil || n != 0 {
		t.Errorf("no volume: %d %v", n, err)
	}
}

func TestExportJobsDisabledWithoutADirectory(t *testing.T) {
	st := newFakeStore()
	s := New(fakeLooker{}, testRegistry(), st, &fakeDecks{}, Config{MaxSavedWords: 10})
	if _, err := s.StartExport(userCtx(), &dictionaryv1.StartExportRequest{Group: "en"}); code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeExportJobsDisabled) {
		t.Errorf("disabled: %v", err)
	}
}

func (f *fakeStore) savedByNames(_ context.Context, _ string, words []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, w := range words {
		if _, ok := f.rows[w]; ok {
			for id := range f.wordCats[w] {
				out[w] = append(out[w], id)
			}
			if _, ok := out[w]; !ok {
				out[w] = nil
			}
		}
	}
	return out, nil
}

// progressDecks reports a fraction of the progress, like the builder of a deck with audio to download.
type progressDecks struct{ limitDecks }

func (d *progressDecks) Build(ctx context.Context, name string, w []anki.Word, fresh bool) ([]byte, error) {
	for _, f := range []float64{0.3, 0.7, 0.95, 0.96} {
		anki.ReportProgress(ctx, f)
	}
	return d.limitDecks.Build(ctx, name, w, fresh)
}

func TestExportJobReportsProgressInsideAPartAndResetsItBetweenParts(t *testing.T) {
	old := buildingEvery
	buildingEvery = 0
	defer func() { buildingEvery = old }()
	decks := &progressDecks{limitDecks{max: 100}}
	s, st := jobService(t, decks, 10)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, _, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 10 || j.Building != 0 {
		t.Fatalf("job: %+v", j)
	}
	// 10 words: 0.3, 0.7, 0.95 and 0.96 of them are done, never the whole part (it shows only when it is recorded)
	want := []int{3, 7, 9}
	got := st.exp.buildingSeen
	if len(got) != len(want) {
		t.Fatalf("written: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("written: %v, want %v", got, want)
		}
	}
}

// A batch that turns out too large is built again smaller: the progress of the page (built + building) never goes down.
func TestExportJobProgressNeverGoesDownWhenABatchIsTooLarge(t *testing.T) {
	old := buildingEvery
	buildingEvery = 0
	defer func() { buildingEvery = old }()
	decks := &progressDecks{limitDecks{max: 4}} // the batch of 10 words is refused after it reported 9 done
	s, st := jobService(t, decks, 10)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, _, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 10 || j.Building != 0 {
		t.Fatalf("job: %+v", j)
	}
	seen := st.exp.shownSeen
	if len(seen) < 3 || seen[0] != 3 {
		t.Fatalf("shown: %v", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("the progress went down: %v", seen)
		}
	}
}

func TestBuildingReporterOnlyGrowsAndIsThrottled(t *testing.T) {
	st := newFakeStore()
	s := New(fakeLooker{}, testRegistry(), st, &fakeDecks{}, Config{MaxSavedWords: 10})
	st.exp.jobs = map[string]*store.ExportJob{"j": {ID: "j", Status: store.JobRunning, LeaseID: "l"}}
	old := buildingEvery
	buildingEvery = time.Hour // after the first write nothing is written for an hour
	defer func() { buildingEvery = old }()
	var shown int
	report := s.buildingReporter(context.Background(), store.ExportJob{ID: "j", LeaseID: "l"}, 100, &shown)
	report(0.5)
	report(0.9)
	if got := st.exp.buildingSeen; len(got) != 1 || got[0] != 50 {
		t.Errorf("throttled: %v", got)
	}
	buildingEvery = 0
	report(0.4) // smaller than what was sent: not written
	report(1.0) // the whole part: one word short, it is recorded by AddExportPart
	if got := st.exp.buildingSeen; len(got) != 2 || got[1] != 99 {
		t.Errorf("growing: %v", got)
	}
}

// chunkStream collects what DownloadExportPart sends.
type chunkStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*dictionaryv1.DownloadChunk
}

func (c *chunkStream) Context() context.Context { return c.ctx }
func (c *chunkStream) Send(m *dictionaryv1.DownloadChunk) error {
	c.chunks = append(c.chunks, &dictionaryv1.DownloadChunk{Filename: m.Filename, Size: m.Size, Data: slices.Clone(m.Data)})
	return nil
}

// download runs DownloadExportPart and joins the chunks, checking the first one alone has the name and the size.
func download(s *Service, ctx context.Context, token string) (string, []byte, error) {
	cs := &chunkStream{ctx: ctx}
	if err := s.DownloadExportPart(&dictionaryv1.DownloadExportPartRequest{Token: token}, cs); err != nil {
		return "", nil, err
	}
	var data []byte
	for i, c := range cs.chunks {
		if (i > 0) && (c.Filename != "" || c.Size != 0) {
			return "", nil, fmt.Errorf("chunk %d repeats the header", i)
		}
		data = append(data, c.Data...)
	}
	if cs.chunks[0].Size != int64(len(data)) {
		return "", nil, fmt.Errorf("size %d, got %d bytes", cs.chunks[0].Size, len(data))
	}
	return cs.chunks[0].Filename, data, nil
}

func TestDownloadExportPartStreamsALargeFileInChunks(t *testing.T) {
	s, st := jobService(t, &limitDecks{max: 3}, 1)
	big := make([]byte, 3*exportChunkSize+17)
	for i := range big {
		big[i] = byte(i * 7)
	}
	job := startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en", DeckName: "Big"})
	runNext(t, s, st)
	_, parts, _ := st.GetExportJob(context.Background(), "", job.Id)
	if err := s.exports.Write(userUUID, parts[0].FileID, "apkg", big); err != nil {
		t.Fatal(err)
	}
	link, err := s.CreateExportPartLink(userCtx(), &dictionaryv1.CreateExportPartLinkRequest{Id: job.Id, N: 1})
	if err != nil {
		t.Fatal(err)
	}
	cs := &chunkStream{ctx: serviceCtx()}
	if err := s.DownloadExportPart(&dictionaryv1.DownloadExportPartRequest{Token: link.Token}, cs); err != nil {
		t.Fatal(err)
	}
	if len(cs.chunks) != 4 {
		t.Fatalf("%d chunks", len(cs.chunks))
	}
	for _, c := range cs.chunks {
		if len(c.Data) > exportChunkSize {
			t.Errorf("chunk of %d bytes", len(c.Data))
		}
	}
	if first := cs.chunks[0]; first.Filename != "Big (part 1).apkg" || first.Size != int64(len(big)) {
		t.Errorf("first chunk: %q %d", first.Filename, first.Size)
	}
	if _, got, err := download(s, serviceCtx(), link.Token); err != nil || !slices.Equal(got, big) {
		t.Errorf("the bytes differ (%d, %v)", len(got), err)
	}
}

// realJobService runs the jobs with the real builder, so the size limit of the settings is what stops a deck.
func realJobService(t *testing.T, words int, content string, cfg Config) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	cfg.MaxSavedWords, cfg.ExportedRetentionHours = 100000, 168
	s := New(fakeLooker{}, testRegistry(), st, &anki.Builder{}, cfg).WithExports(&filestore.Store{Dir: t.TempDir()})
	for i := range words {
		c := content
		if len(c) > 8 { // random text of that length: the zip cannot squeeze it away
			var b strings.Builder
			for b.Len() < len(content) {
				b.WriteString(strings.ReplaceAll(uuid.NewString(), "-", ""))
			}
			c = b.String()
		}
		st.rows[fmt.Sprintf("w%05d", i)] = store.SavedWord{Word: fmt.Sprintf("w%05d", i), Content: c, CreatedAt: st.clock.Add(time.Duration(i) * time.Second)}
	}
	return s, st
}

func putExportLimit(st *fakeStore, key string, n int) {
	if st.settings == nil {
		st.settings = map[string]json.RawMessage{}
	}
	st.settings[key] = json.RawMessage(fmt.Sprint(n))
}

func TestExportJobBatchGrowsWhileThePartsStayUnderHalfTheLimit(t *testing.T) {
	s, st := realJobService(t, 5000, "c", Config{ExportPartMaxBytes: 8 << 20, ExportPartMaxBytesCeiling: 8 << 20})
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, parts, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 5000 {
		t.Fatalf("job: %+v", j)
	}
	if len(parts) != 2 || parts[0].WordCount != jobBatch || parts[1].WordCount != 3000 {
		t.Errorf("parts: %+v", parts)
	}
}

func TestExportJobBatchStopsAtTheConfiguredMaximum(t *testing.T) {
	s, st := realJobService(t, 5000, "c", Config{ExportPartMaxBytes: 8 << 20, ExportPartMaxBytesCeiling: 8 << 20, ExportJobBatchMax: jobBatch})
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	if _, parts, _ := st.GetExportJob(context.Background(), "", "latest"); len(parts) != 3 {
		t.Errorf("%d parts, want 3 of at most %d words", len(parts), jobBatch)
	}
}

func TestExportJobReadsThePartLimitOfTheSettingForEveryBatch(t *testing.T) {
	// 64 KiB parts of 2 KiB cards: dozens of words per part, never one batch of 2000
	s, st := realJobService(t, 400, strings.Repeat("x", 2000), Config{ExportPartMaxBytes: 64 << 10, ExportPartMaxBytesCeiling: 1 << 20})
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, parts, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 400 {
		t.Fatalf("job: %+v", j)
	}
	for _, p := range parts {
		if p.Size > 64<<10 {
			t.Errorf("part %d is %d bytes, over the limit", p.N, p.Size)
		}
	}
	small := len(parts)

	// the administrator raises the limit while a job runs: the next batch is packed under the new one
	s2, st2 := realJobService(t, 400, strings.Repeat("x", 2000), Config{ExportPartMaxBytes: 64 << 10, ExportPartMaxBytesCeiling: 1 << 20})
	putExportLimit(st2, "export_part_max_bytes", 1<<20)
	startJob(t, s2, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s2, st2)
	if _, parts, _ := st2.GetExportJob(context.Background(), "", "latest"); len(parts) >= small || len(parts) < 1 {
		t.Errorf("a larger limit gave %d parts, the small one %d", len(parts), small)
	}
}

func TestBuildDeckUsesTheExportSizeOfTheSetting(t *testing.T) {
	s, st := realJobService(t, 100, strings.Repeat("y", 2000), Config{ExportMaxBytes: 4 << 20, ExportMaxBytesCeiling: 4 << 20})
	putExportLimit(st, "export_max_bytes", 64<<10)
	_, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"})
	if code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), CodeExportTooBig) {
		t.Fatalf("a deck over the setting: %v", err)
	}
	putExportLimit(st, "export_max_bytes", 4<<20)
	if _, err := s.ExportAnki(userCtx(), &dictionaryv1.ExportAnkiRequest{Group: "en"}); err != nil {
		t.Errorf("the same deck under the raised setting: %v", err)
	}
}

// dictionary-worker builds the service without a looker: a job never looks a word up.
func TestExportJobRunsWithoutALooker(t *testing.T) {
	st := newFakeStore()
	s := New(nil, testRegistry(), st, &fakeDecks{}, Config{MaxSavedWords: 100000, ExportedRetentionHours: 168}).WithExports(&filestore.Store{Dir: t.TempDir()})
	for i := range 3 {
		st.rows[fmt.Sprintf("w%03d", i)] = store.SavedWord{Word: fmt.Sprintf("w%03d", i), Content: "c", CreatedAt: st.clock.Add(time.Duration(i) * time.Second)}
	}
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, parts, err := st.GetExportJob(context.Background(), "", "latest")
	if err != nil || j.Status != store.JobDone || j.Built != 3 || len(parts) != 1 {
		t.Fatalf("job: %+v parts %d err %v", j, len(parts), err)
	}
}

// weightDecks builds a deck of a size that depends on the word: heavy(i) bytes for the word w<i>. It records every
// batch it was asked for and refuses one over max bytes, like the real builder.
type weightDecks struct {
	max     int
	heavy   func(i int) int
	batches []int
	refused int
}

func (d *weightDecks) Build(_ context.Context, _ string, w []anki.Word, _ bool) ([]byte, error) {
	d.batches = append(d.batches, len(w))
	size := 0
	for _, x := range w {
		var i int
		_, _ = fmt.Sscanf(x.Word, "w%d", &i)
		size += d.heavy(i)
	}
	if size > d.max {
		d.refused++
		return nil, anki.ErrTooLarge
	}
	return make([]byte, size), nil
}

func TestExportJobBatchIsSizedFromTheBytesPerWordOfThePart(t *testing.T) {
	// 100 bytes a word, parts of at most 300000: the first batch is 2000 words (200000 bytes), then 2400 (80% of the limit)
	decks := &weightDecks{max: 300_000, heavy: func(int) int { return 100 }}
	s, st := jobService(t, decks, 7000)
	putExportLimit(st, "export_part_max_bytes", 300_000)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, parts, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 7000 {
		t.Fatalf("job: %+v", j)
	}
	if want := []int{2000, 2400, 2400, 200}; !slices.Equal(decks.batches, want) || decks.refused != 0 || len(parts) != 4 {
		t.Errorf("batches %v (want %v), refused %d, %d parts", decks.batches, want, decks.refused, len(parts))
	}
}

func TestExportJobBatchShrinksWhenTheWordsGetHeavier(t *testing.T) {
	// the words after the 2000th are four times heavier: the batch built from the light ones is refused and halved
	// until it fits, then every batch is sized from the heavy ones and none is built twice
	decks := &weightDecks{max: 300_000, heavy: func(i int) int {
		if i < 2000 {
			return 100
		}
		return 400
	}}
	s, st := jobService(t, decks, 5000)
	putExportLimit(st, "export_part_max_bytes", 300_000)
	startJob(t, s, &dictionaryv1.StartExportRequest{Group: "en"})
	runNext(t, s, st)
	j, _, _ := st.GetExportJob(context.Background(), "", "latest")
	if j.Status != store.JobDone || j.Built != 5000 {
		t.Fatalf("job: %+v", j)
	}
	if decks.refused != 2 {
		t.Errorf("%d batches refused (batches %v), want 2: 2400 and 1200 words, then 600 fit", decks.refused, decks.batches)
	}
}
