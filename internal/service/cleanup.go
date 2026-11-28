package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/logging"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
	"go.uber.org/zap"
)

// TaskCleanup is the kind of the task that runs the cleanup of dictionary-worker now (table async_tasks, gocommon/tasks).
const TaskCleanup = "cleanup"

// CleanupResult is what a cleanup task deleted, the JSON in the result of its row.
type CleanupResult struct {
	Words        int64 `json:"words"`
	Images       int   `json:"images"`
	Audio        int   `json:"audio"`
	OrphanImages int   `json:"orphan_images"`
	OrphanAudio  int   `json:"orphan_audio"`
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func cleanupRun(ctx context.Context, t store.Task) *dictionaryv1.CleanupRun {
	var r CleanupResult
	if len(t.Result) > 0 {
		if err := json.Unmarshal(t.Result, &r); err != nil {
			// the counts are lost, the state is not
			logging.FromCtx(ctx).Warn("decode cleanup result", zap.Error(err))
		}
	}
	return &dictionaryv1.CleanupRun{
		Status: t.Status, RequestedAt: unixOrZero(t.RequestedAt), StartedAt: unixOrZero(t.StartedAt), FinishedAt: unixOrZero(t.FinishedAt),
		Words: r.Words, Images: int32(r.Images), Audio: int32(r.Audio),
		OrphanImages: int32(r.OrphanImages), OrphanAudio: int32(r.OrphanAudio), Error: t.Error,
	}
}

// RunCleanup asks dictionary-worker to run its cleanup now and returns at once; the worker takes the request within
// seconds (package tasks). A request that is pending or running already stays.
func (s *Service) RunCleanup(ctx context.Context, _ *dictionaryv1.RunCleanupRequest) (*dictionaryv1.RunCleanupResponse, error) {
	t, _, err := s.store.RequestTask(ctx, TaskCleanup)
	if err != nil {
		return nil, internal(ctx, "request cleanup", err)
	}
	return &dictionaryv1.RunCleanupResponse{Run: cleanupRun(ctx, t)}, nil
}

// GetCleanupStatus returns the latest cleanup that was asked for.
func (s *Service) GetCleanupStatus(ctx context.Context, _ *dictionaryv1.GetCleanupStatusRequest) (*dictionaryv1.GetCleanupStatusResponse, error) {
	t, err := s.store.TaskStatus(ctx, TaskCleanup)
	if err != nil {
		return nil, internal(ctx, "cleanup status", err)
	}
	return &dictionaryv1.GetCleanupStatusResponse{Run: cleanupRun(ctx, t)}, nil
}
