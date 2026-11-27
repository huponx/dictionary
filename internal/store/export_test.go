package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests need V30 (the export jobs) next to the other migrations.

func saveN(t *testing.T, s *Store, user string, n int) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO dictionary_saved_words (user_id, grp, word, content, created_at)
		SELECT $1, 'en', 'w' || lpad(g::text, 6, '0'), '', now() - make_interval(secs => $2 - g) FROM generate_series(1, $2) g`, user, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO dictionary_user_stats (user_id, listed) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET listed = EXCLUDED.listed`, user, n); err != nil {
		t.Fatal(err)
	}
}

func TestPendingPageKeyset(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	saveN(t, s, user, 7)
	var got []string
	var after SavedWord
	for {
		page, err := s.PendingPage(ctx, user, ScopeNew, WordFilter{Group: "en"}, after.CreatedAt, after.Word, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range page {
			got = append(got, w.Word)
		}
		if len(page) < 3 {
			break
		}
		after = page[len(page)-1]
	}
	if len(got) != 7 || got[0] != "w000001" || got[6] != "w000007" {
		t.Errorf("pages (oldest first): %v", got)
	}
	if n, err := s.CountExportable(ctx, user, ScopeNew, WordFilter{Group: "en"}); err != nil || n != 7 {
		t.Errorf("CountExportable: %d %v", n, err)
	}
}

func TestExportJobLifecycle(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	saveN(t, s, user, 5)
	// 10 bytes of content a word, and the counter of them
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_saved_words SET content = repeat('x', 10) WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_user_stats SET content_bytes = 50 WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	p := ExportParams{Group: "en", DeckName: "D", Remove: true}
	job, err := s.CreateExportJob(ctx, user, p, time.Hour)
	if err != nil || job.Status != JobQueued {
		t.Fatalf("create: %+v %v", job, err)
	}
	if _, err := s.CreateExportJob(ctx, user, p, time.Hour); !errors.Is(err, ErrJobActive) {
		t.Errorf("second job: %v", err)
	}
	// take it: only this user's job matters, other users' jobs in a shared database may be taken first, so claim until ours
	var claimed *ExportJob
	for range 100 {
		c, err := s.ClaimExportJob(ctx, time.Minute)
		if err != nil || c == nil {
			break
		}
		if c.ID == job.ID {
			claimed = c
			break
		}
		_ = s.FinishExportJob(ctx, c.ID, c.LeaseID, JobFailed, "test cleanup", time.Hour)
	}
	if claimed == nil || claimed.LeaseID == "" {
		t.Fatalf("claim: %+v", claimed)
	}
	if err := s.SetExportTotal(ctx, claimed.ID, claimed.LeaseID, 5); err != nil {
		t.Fatal(err)
	}
	words, _ := s.PendingPage(ctx, user, ScopeNew, WordFilter{Group: "en"}, time.Time{}, "", 3)
	keys := []string{words[0].Word, words[1].Word, words[2].Word}
	part := ExportPart{N: 1, Filename: "D (part 1).apkg", FileID: uuid.NewString(), WordCount: 3, Size: 10}
	if err := s.AddExportPart(ctx, *claimed, claimed.LeaseID, part, keys, words[2]); err != nil {
		t.Fatal(err)
	}
	// the three words left the list with the part, and the counter followed
	if c, r := listedOf(t, s, user); c != 2 || r != 2 {
		t.Errorf("counter %d, count(*) %d, want 2", c, r)
	}
	if c, r := contentBytesOf(t, s, user); c != 20 || r != 20 {
		t.Errorf("content bytes: counter %d, rows %d: the export did not move them with the words", c, r)
	}
	// a wrong lease and a wrong part number write nothing
	if err := s.AddExportPart(ctx, *claimed, uuid.NewString(), ExportPart{N: 2, FileID: uuid.NewString()}, nil, words[2]); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("wrong lease: %v", err)
	}
	if err := s.AddExportPart(ctx, *claimed, claimed.LeaseID, ExportPart{N: 5, FileID: uuid.NewString()}, nil, words[2]); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("wrong part number: %v", err)
	}
	// the job goes on after the last word packed (the key is in the params): another replica reads it back
	j, parts, err := s.GetExportJob(ctx, user, "latest")
	if err != nil || j.Built != 3 || j.Parts != 1 || len(parts) != 1 || j.Params.AfterWord != "w000003" || !j.Params.Remove {
		t.Fatalf("job after a part: %+v %v", j, err)
	}
	if err := s.FinishExportJob(ctx, claimed.ID, claimed.LeaseID, JobDone, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewExportLease(ctx, claimed.ID, claimed.LeaseID, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("renew after the end: %v", err)
	}
	// a finished job lets the user start another
	if _, err := s.CreateExportJob(ctx, user, p, time.Hour); err != nil {
		t.Errorf("job after a finished one: %v", err)
	}
}

func TestExportJobTakenOverAfterTheLeaseEnds(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	job, err := s.CreateExportJob(ctx, user, ExportParams{Group: "en", DeckName: "D"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// the first replica takes it and dies: its lease ends
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET status = 'running', lease_id = gen_random_uuid(), lease_until = now() - interval '1 second' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	var second *ExportJob
	for range 100 {
		c, err := s.ClaimExportJob(ctx, time.Minute)
		if err != nil || c == nil {
			break
		}
		if c.ID == job.ID {
			second = c
			break
		}
		_ = s.FinishExportJob(ctx, c.ID, c.LeaseID, JobFailed, "test cleanup", time.Hour)
	}
	if second == nil {
		t.Fatal("the job whose lease ended was not taken")
	}
	// a running job with a live lease is not taken by a third replica
	if c, _ := s.ClaimExportJob(ctx, time.Minute); c != nil && c.ID == job.ID {
		t.Error("a job with a live lease was taken twice")
	}
}

func TestExportJobCancel(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	job, _ := s.CreateExportJob(ctx, user, ExportParams{Group: "en"}, time.Hour)
	c, err := s.CancelExportJob(ctx, user, job.ID, time.Hour)
	if err != nil || c.Status != JobCancelled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if again, err := s.CancelExportJob(ctx, user, job.ID, time.Hour); err != nil || again.Status != JobCancelled {
		t.Errorf("second cancel: %+v %v", again, err)
	}
	other, otherUser := testStore(t)
	_ = other
	if _, err := s.CancelExportJob(ctx, otherUser, job.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("the job of another user: %v", err)
	}
	if _, _, err := s.GetExportJob(ctx, otherUser, job.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get the job of another user: %v", err)
	}
}

func TestExportPartTokenAndExpiry(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	job, _ := s.CreateExportJob(ctx, user, ExportParams{Group: "en"}, time.Hour)
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET status = 'done' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	file := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_export_parts (job_id, n, filename, file_id, word_count, size) VALUES ($1, 1, 'p.apkg', $2, 3, 9)`, job.ID, file); err != nil {
		t.Fatal(err)
	}
	hash := []byte(fmt.Sprintf("hash-%s", job.ID))
	if _, _, err := s.SetExportPartToken(ctx, user, job.ID, 2, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown part: %v", err)
	}
	p, expires, err := s.SetExportPartToken(ctx, user, job.ID, 1, hash)
	if err != nil || p.FileID != file || !expires.After(time.Now()) {
		t.Fatalf("token: %+v %v", p, err)
	}
	for range 3 { // a link works many times
		if u, got, err := s.ExportPartByToken(ctx, hash, time.Now()); err != nil || u != user || got.FileID != file {
			t.Fatalf("by token: %s %+v %v", u, got, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dictionary_export_jobs SET expires_at = now() - interval '1 minute' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ExportPartByToken(ctx, hash, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the job expired: %v", err)
	}
	n, files, err := s.DeleteExpiredExportJobs(ctx, time.Now(), 1000)
	if err != nil || n < 1 {
		t.Fatalf("delete expired: %d %v", n, err)
	}
	found := false
	for _, f := range files {
		found = found || (f.FileID == file && f.UserID == user)
	}
	if !found {
		t.Errorf("the file of the part is not returned: %+v", files)
	}
	if _, _, err := s.GetExportJob(ctx, user, job.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the job is still there: %v", err)
	}
}

func TestExportBuildingProgress(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	saveN(t, s, user, 3)
	job, err := s.CreateExportJob(ctx, user, ExportParams{Group: "en", DeckName: "D"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var claimed *ExportJob
	for range 100 {
		c, err := s.ClaimExportJob(ctx, time.Minute)
		if err != nil || c == nil {
			break
		}
		if c.ID == job.ID {
			claimed = c
			break
		}
		_ = s.FinishExportJob(ctx, c.ID, c.LeaseID, JobFailed, "test cleanup", time.Hour)
	}
	if claimed == nil {
		t.Fatal("not claimed")
	}
	reading := func() int {
		j, _, err := s.GetExportJob(ctx, user, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return j.Building
	}
	if err := s.SetExportBuilding(ctx, claimed.ID, claimed.LeaseID, 2); err != nil || reading() != 2 {
		t.Fatalf("building = %d, %v", reading(), err)
	}
	// a replica that lost the lease changes nothing
	if err := s.SetExportBuilding(ctx, claimed.ID, uuid.NewString(), 3); err != nil || reading() != 2 {
		t.Errorf("another lease: building = %d, %v", reading(), err)
	}
	// recording the part resets it, and so does the end of the job
	words, _ := s.PendingPage(ctx, user, ScopeNew, WordFilter{Group: "en"}, time.Time{}, "", 2)
	part := ExportPart{N: 1, Filename: "p.apkg", FileID: uuid.NewString(), WordCount: 2, Size: 1}
	if err := s.AddExportPart(ctx, *claimed, claimed.LeaseID, part, []string{words[0].Word, words[1].Word}, words[1]); err != nil {
		t.Fatal(err)
	}
	if reading() != 0 {
		t.Errorf("after the part: building = %d", reading())
	}
	_ = s.SetExportBuilding(ctx, claimed.ID, claimed.LeaseID, 1)
	if err := s.FinishExportJob(ctx, claimed.ID, claimed.LeaseID, JobFailed, "x", time.Hour); err != nil || reading() != 0 {
		t.Errorf("after the end: building = %d, %v", reading(), err)
	}
}

func TestExistingExportFileIDs(t *testing.T) {
	ctx := context.Background()
	s, user := testStore(t)
	saveN(t, s, user, 2)
	job, err := s.CreateExportJob(ctx, user, ExportParams{Group: "en", DeckName: "D"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ExistingExportFileIDs(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids: %v %v", got, err)
	}
	recorded, unknown := uuid.NewString(), uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO dictionary_export_parts (job_id, n, filename, file_id, word_count, size)
		VALUES ($1, 1, 'D (part 1).apkg', $2::uuid, 2, 10)`, job.ID, recorded); err != nil {
		t.Fatal(err)
	}
	got, err := s.ExistingExportFileIDs(ctx, []string{recorded, job.ID, unknown})
	if err != nil || len(got) != 1 || !got[recorded] {
		t.Errorf("only the recorded part file exists: %v %v", got, err)
	}
}
