package service

import (
	"encoding/json"
	"testing"

	"github.com/huponx/dictionary/internal/store"
	"github.com/huponx/gocommon/tasks"
	dictionaryv1 "github.com/huponx/protogen/go/dictionary/v1"
)

func TestRunCleanupReturnsAtOnceWithTheRequest(t *testing.T) {
	st := newFakeStore()
	s := newService(st, &fakeDecks{})
	if g, err := s.GetCleanupStatus(userCtx(), &dictionaryv1.GetCleanupStatusRequest{}); err != nil || g.Run.Status != "" {
		t.Fatalf("before any request: %+v, %v", g, err)
	}
	r, err := s.RunCleanup(userCtx(), &dictionaryv1.RunCleanupRequest{})
	if err != nil || r.Run.Status != "pending" || r.Run.RequestedAt != 1000 || r.Run.FinishedAt != 0 {
		t.Fatalf("request: %+v, %v", r, err)
	}
	// asked again while pending: the same request stays
	if r2, err := s.RunCleanup(userCtx(), &dictionaryv1.RunCleanupRequest{}); err != nil || r2.Run.Status != "pending" {
		t.Fatalf("second request: %+v, %v", r2, err)
	}
	// what the worker recorded comes back through the status
	st.tasks[TaskCleanup] = store.Task{Kind: TaskCleanup, Status: tasks.Done,
		Result: json.RawMessage(`{"words":3,"images":2,"audio":1,"orphan_images":4,"orphan_audio":5}`)}
	g, err := s.GetCleanupStatus(userCtx(), &dictionaryv1.GetCleanupStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if x := g.Run; x.Status != "done" || x.Words != 3 || x.Images != 2 || x.Audio != 1 || x.OrphanImages != 4 || x.OrphanAudio != 5 {
		t.Fatalf("status: %+v", x)
	}
}
