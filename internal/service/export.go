package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/dictionary/internal/anki"
	"github.com/huponx/dictionary/internal/filestore"
	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/logging"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	// jobBatch is the number of words a job starts with. After every full part the next batch is sized from the bytes
	// per word of that part, to fill jobFill of the size limit, at most twice as large as the one before and at most
	// ExportJobBatchMax. A part of a job is at most one batch (fewer when the deck would pass the size limit). It
	// bounds the memory of a job.
	jobBatch = 2000
	// jobFill is the share of the size limit a part is aimed at: the words of a part are not all alike, so the aim
	// leaves room and a batch is rarely built twice.
	jobFill = 0.8
	// defaultJobBatchMax is ExportJobBatchMax when the configuration sets none.
	defaultJobBatchMax = 20000
	// jobLease is how long a replica owns a running job without renewing; a dead replica's job is taken again after it.
	jobLease = 60 * time.Second
	// jobPoll is how often an idle worker looks for a job.
	jobPoll = 3 * time.Second
	// maxJobsPerInstance is how many jobs one replica runs at once.
	maxJobsPerInstance = 2
)

// buildingEvery is how often the progress inside a part is written (a variable: the tests do not wait).
var buildingEvery = time.Second

// WithExports turns the export jobs on: the part files go to files (one directory of the exports). Without it
// StartExport fails with EXPORT_JOBS_DISABLED and no job runs.
func (s *Service) WithExports(files *filestore.Store) *Service {
	s.exports = files
	return s
}

func toProtoJob(j store.ExportJob, parts []store.ExportPart) *dictionaryv1.ExportJob {
	out := &dictionaryv1.ExportJob{
		Id: j.ID, Status: j.Status, DeckName: j.Params.DeckName, Total: int32(j.Total), Built: int32(j.Built), Building: int32(j.Building), Error: j.Error,
		CreatedAt: j.CreatedAt.UTC().Format(time.RFC3339), ExpiresAt: j.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if !j.FinishedAt.IsZero() {
		out.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	for _, p := range parts {
		out.Parts = append(out.Parts, &dictionaryv1.ExportPart{N: int32(p.N), Filename: p.Filename, WordCount: int32(p.WordCount), Size: int32(p.Size)})
	}
	return out
}

func exportJobErr(ctx context.Context, what string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperror.NotFound("export not found").WithReason("export_not_found")
	case errors.Is(err, store.ErrJobActive):
		return apperror.FailedPrecondition(CodeExportJobActive + ": an export is already running, wait for it or cancel it").WithReason("export_job_active")
	case errors.Is(err, store.ErrUnknownUser):
		return apperror.Unauthenticated("unknown user").WithReason("unknown_user")
	}
	return internal(ctx, what, err)
}

// StartExport queues a job that packs the words in parts (see the proto).
func (s *Service) StartExport(ctx context.Context, req *dictionaryv1.StartExportRequest) (*dictionaryv1.StartExportResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	sys, err := s.requireExport(ctx, false)
	if err != nil {
		return nil, err
	}
	if s.exports == nil {
		return nil, apperror.FailedPrecondition(CodeExportJobsDisabled + ": exports in parts are not available").WithReason("export_jobs_disabled")
	}
	name, err := deckName(req.GetDeckName())
	if err != nil {
		return nil, err
	}
	filter := wordFilter(req.GetCategoryId(), req.GetUncategorized())
	if filter.CategoryID != "" {
		catName, err := s.store.CategoryName(ctx, user, filter.CategoryID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, apperror.NotFound("category not found").WithReason("category_not_found")
		}
		if err != nil {
			return nil, internal(ctx, "read category", err)
		}
		if strings.TrimSpace(req.GetDeckName()) == "" {
			name = catName
		}
	}
	scope := storeScope(req.GetScope())
	if first, err := s.store.PendingPage(ctx, user, scope, filter, time.Time{}, "", 1); err != nil {
		return nil, internal(ctx, "read pending words", err)
	} else if len(first) == 0 {
		return nil, apperror.InvalidArgument(CodeNoWords + ": no saved words to export").WithReason("no_words_to_export")
	}
	p := store.ExportParams{
		All: scope == store.ScopeAll, CategoryID: filter.CategoryID, Uncategorized: filter.Uncategorized, DeckName: name,
		Fresh: s.userPrefs(ctx).FreshDeck, Remove: s.removesAfterExport(ctx, sys),
	}
	j, err := s.store.CreateExportJob(ctx, user, p, s.cfg.ExportJobTTL)
	if err != nil {
		return nil, exportJobErr(ctx, "create export job", err)
	}
	return &dictionaryv1.StartExportResponse{Job: toProtoJob(j, nil)}, nil
}

// GetExport returns a job with its parts.
func (s *Service) GetExport(ctx context.Context, req *dictionaryv1.GetExportRequest) (*dictionaryv1.GetExportResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	j, parts, err := s.store.GetExportJob(ctx, user, req.GetId())
	if err != nil {
		return nil, exportJobErr(ctx, "get export job", err)
	}
	return &dictionaryv1.GetExportResponse{Job: toProtoJob(j, parts)}, nil
}

// CancelExport stops a job. Its words that are in a finished part stay exported.
func (s *Service) CancelExport(ctx context.Context, req *dictionaryv1.CancelExportRequest) (*dictionaryv1.CancelExportResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	defer s.cats.forget(user)
	j, err := s.store.CancelExportJob(ctx, user, req.GetId(), s.cfg.ExportJobTTL)
	if err != nil {
		return nil, exportJobErr(ctx, "cancel export job", err)
	}
	_, parts, err := s.store.GetExportJob(ctx, user, j.ID)
	if err != nil {
		return nil, exportJobErr(ctx, "get export job", err)
	}
	return &dictionaryv1.CancelExportResponse{Job: toProtoJob(j, parts)}, nil
}

// CreateExportPartLink makes the link of a part; it works until the job expires.
func (s *Service) CreateExportPartLink(ctx context.Context, req *dictionaryv1.CreateExportPartLinkRequest) (*dictionaryv1.CreateExportPartLinkResponse, error) {
	user, err := userID(ctx)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, internal(ctx, "token", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	p, expires, err := s.store.SetExportPartToken(ctx, user, req.GetId(), int(req.GetN()), tokenHash(token))
	if err != nil {
		return nil, exportJobErr(ctx, "export part link", err)
	}
	return &dictionaryv1.CreateExportPartLinkResponse{
		Token: token, Filename: p.Filename, WordCount: int32(p.WordCount), ExpiresAt: expires.UTC().Format(time.RFC3339),
	}, nil
}

// exportChunkSize is the size of a chunk of DownloadExportPart.
const exportChunkSize = 256 << 10

// DownloadExportPart streams the part of a job behind the token: the first chunk has the file name and the size,
// the others the bytes. Only a service token (the gateway) may call it; the token is the whole authorization.
func (s *Service) DownloadExportPart(req *dictionaryv1.DownloadExportPartRequest, stream grpc.ServerStreamingServer[dictionaryv1.DownloadChunk]) error {
	ctx := stream.Context()
	if p := authn.FromContext(ctx); p == nil || !p.IsService() {
		return apperror.PermissionDenied("a service token is required").WithReason("service_token_required")
	}
	gone := apperror.NotFound("the download link does not exist or has expired").WithReason("download_link_gone")
	token := req.GetToken()
	if s.exports == nil || len(token) < 20 || len(token) > 128 {
		return gone
	}
	user, p, err := s.store.ExportPartByToken(ctx, tokenHash(token), time.Now())
	if errors.Is(err, store.ErrNotFound) {
		return gone
	}
	if err != nil {
		return internal(ctx, "export part by token", err)
	}
	f, size, err := s.exports.Open(user, p.FileID, "apkg")
	if errors.Is(err, filestore.ErrNotFound) {
		return gone
	}
	if err != nil {
		return internal(ctx, "open export part", err)
	}
	defer f.Close()
	first := true
	buf := make([]byte, exportChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 || first {
			chunk := &dictionaryv1.DownloadChunk{Data: buf[:n]}
			if first {
				chunk.Filename, chunk.Size = p.Filename, size
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return internal(ctx, "read export part", rerr)
		}
	}
}

// RunExportWorker runs the export jobs of the queue until ctx ends: it takes a job (the oldest queued, or one whose
// replica died), at most maxJobsPerInstance at once. Several replicas can run it.
func (s *Service) RunExportWorker(ctx context.Context) {
	if s.exports == nil {
		return
	}
	log := logging.FromCtx(ctx)
	slots := make(chan struct{}, maxJobsPerInstance)
	t := time.NewTicker(jobPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case slots <- struct{}{}:
		}
		job, err := s.store.ClaimExportJob(ctx, jobLease)
		if err != nil || job == nil {
			<-slots
			if err != nil && ctx.Err() == nil {
				log.Warn("claim export job", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			continue
		}
		go func() {
			defer func() { <-slots }()
			s.runExportJob(ctx, *job)
		}()
	}
}

// runExportJob packs the words of the job in parts until none is left. It owns the job under job.LeaseID and stops,
// without touching it, when it loses the lease (cancelled, deleted, taken over) or ctx ends (the lease runs out and
// another replica goes on after the last part).
func (s *Service) runExportJob(ctx context.Context, job store.ExportJob) {
	log := logging.FromCtx(ctx).With(zap.String("job", job.ID), zap.String("user", job.UserID))
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // keep the lease while a long batch builds
		t := time.NewTicker(jobLease / 3)
		defer t.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-t.C:
				if err := s.store.RenewExportLease(jobCtx, job.ID, job.LeaseID, jobLease); err != nil {
					if errors.Is(err, store.ErrLeaseLost) {
						cancel()
					}
					if jobCtx.Err() == nil {
						log.Warn("renew export lease", zap.Error(err))
					}
				}
			}
		}
	}()
	finish := func(status, msg string) {
		if err := s.store.FinishExportJob(ctx, job.ID, job.LeaseID, status, msg, s.cfg.ExportJobTTL); err != nil && !errors.Is(err, store.ErrLeaseLost) {
			log.Warn("finish export job", zap.Error(err))
		}
	}
	fail := func(what string, err error) {
		if jobCtx.Err() != nil { // cancelled or shutting down: not a failure of the job
			return
		}
		log.Error(what, zap.Error(err))
		finish(store.JobFailed, what)
	}

	p := job.Params
	if job.Total == 0 {
		total, err := s.store.CountExportable(jobCtx, job.UserID, p.Scope(), p.Filter())
		if err != nil {
			fail("count the words", err)
			return
		}
		if err := s.store.SetExportTotal(jobCtx, job.ID, job.LeaseID, total); err != nil {
			fail("save the total", err)
			return
		}
	}
	base := strings.TrimSuffix(fileName(p.DeckName), ".apkg")
	limit, batchMax := jobBatch, s.jobBatchMax()
	// shown is the progress inside the part that the job row holds. The page shows built + building and it must not go
	// down: a batch that turns out too large is built again smaller, and a finished part leaves what the next one has
	// not reached yet.
	shown := job.Building
	for jobCtx.Err() == nil {
		// the limit is read for every batch: a change of the setting applies to the next part
		sys, err := s.system(jobCtx)
		if err != nil {
			fail("read the settings", err)
			return
		}
		words, err := s.store.PendingPage(jobCtx, job.UserID, p.Scope(), p.Filter(), p.AfterCreated, p.AfterWord, limit)
		if err != nil {
			fail("read the words", err)
			return
		}
		if len(words) == 0 {
			finish(store.JobDone, "")
			return
		}
		data, err := s.packDeck(anki.WithProgress(jobCtx, s.buildingReporter(jobCtx, job, len(words), &shown)), job.UserID, p.DeckName, words, p.Fresh, sys.ExportPartMaxBytes)
		if errors.Is(err, errDeckTooLarge) {
			if len(words) == 1 {
				fail(fmt.Sprintf("the card of %q is too large for a deck", words[0].Word), err)
				return
			}
			limit = len(words) / 2 // read a smaller batch; the next ones are sized from the parts that fit
			continue
		}
		if err != nil {
			fail("build the deck", err)
			return
		}
		n := job.Parts + 1
		part := store.ExportPart{
			N: n, Filename: fmt.Sprintf("%s (part %d).apkg", base, n), FileID: uuid.NewString(), WordCount: len(words), Size: len(data),
		}
		if err := s.exports.Write(job.UserID, part.FileID, "apkg", data); err != nil {
			fail("write the part", err)
			return
		}
		keys := make([]string, len(words))
		for i, w := range words {
			keys[i] = w.Word
		}
		last := words[len(words)-1]
		if err := s.store.AddExportPart(jobCtx, job, job.LeaseID, part, keys, last); err != nil {
			_ = s.exports.Remove(job.UserID, part.FileID, "apkg") // the part is not recorded: its file is an orphan
			if !errors.Is(err, store.ErrLeaseLost) {
				fail("record the part", err)
			}
			return
		}
		job.Parts = n
		shown = max(shown-len(words), 0)
		p.AfterCreated, p.AfterWord = last.CreatedAt, last.Word
		// a full batch tells the bytes per word: the next one is sized to fill jobFill of the limit (a heavier part
		// makes it smaller, a lighter one larger, up to twice)
		if len(words) == limit && len(data) > 0 {
			fit := int(jobFill * float64(sys.ExportPartMaxBytes) * float64(len(words)) / float64(len(data)))
			limit = max(min(fit, limit*2, batchMax), 1)
		}
	}
}

func (s *Service) jobBatchMax() int {
	if s.cfg.ExportJobBatchMax > 0 {
		return max(s.cfg.ExportJobBatchMax, jobBatch)
	}
	return defaultJobBatchMax
}

// buildingReporter turns the fraction Build reports into the words of the part that are done and writes it to the job, at
// most once a second and only when it grew past *shown (what the row holds now, which a retry with a smaller batch
// starts with: the number on the page never goes down), so the page shows the progress inside a long part. A part
// never shows as finished before it is recorded (it stops one word short). An error is ignored: the progress is only
// for the eyes.
func (s *Service) buildingReporter(ctx context.Context, job store.ExportJob, words int, shown *int) func(float64) {
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(fraction float64) {
		n := min(int(fraction*float64(words)), words-1)
		mu.Lock()
		defer mu.Unlock()
		if n <= *shown || time.Since(last) < buildingEvery {
			return
		}
		if err := s.store.SetExportBuilding(ctx, job.ID, job.LeaseID, n); err != nil {
			return
		}
		*shown, last = n, time.Now()
	}
}

// CleanExports deletes the jobs that expired and their files; it returns how many jobs it deleted. The cleanup of
// the service calls it every tick.
func (s *Service) CleanExports(ctx context.Context, batch int) (int, error) {
	if s.exports == nil {
		return 0, nil
	}
	total := 0
	for {
		n, files, err := s.store.DeleteExpiredExportJobs(ctx, time.Now(), batch)
		if err != nil {
			return total, err
		}
		for _, f := range files {
			if err := s.exports.Remove(f.UserID, f.FileID, "apkg"); err != nil {
				logging.FromCtx(ctx).Warn("remove export part", zap.String("file", f.FileID), zap.Error(err))
			}
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// SweepExportFiles deletes the files of the exports volume that no part records: a worker that died after it wrote
// a part and before it recorded it leaves one. Files younger than minAge are kept (a part being written is not
// recorded yet). It returns how many files it deleted.
func (s *Service) SweepExportFiles(ctx context.Context, minAge time.Duration, batch int) (int, error) {
	if s.exports == nil {
		return 0, nil
	}
	users, err := s.exports.Users()
	if err != nil {
		return 0, err
	}
	total := 0
	var firstErr error
	for _, u := range users {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		n, err := s.exports.Orphans(u, minAge, batch, func(fs []filestore.File) (map[string]bool, error) {
			ids := make([]string, len(fs))
			for i, f := range fs {
				ids[i] = f.ID
			}
			return s.store.ExistingExportFileIDs(ctx, ids)
		})
		total += n
		if err != nil {
			logging.FromCtx(ctx).Warn("sweep export files", zap.String("user", u), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return total, firstErr
}
