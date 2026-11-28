package store

import (
	"context"

	"github.com/huponx/gocommon/tasks"
)

// TaskService is the name of this service in the table async_tasks (gocommon/tasks).
const TaskService = "dictionary"

// Task is the latest request of one kind of task (see gocommon/tasks).
type Task = tasks.Task

// Tasks is what the API and the worker share to ask for and run tasks.
func (s *Store) Tasks() *tasks.Store { return tasks.New(s.pool, TaskService) }

// RequestTask asks for a task of the kind; see tasks.Store.Request.
func (s *Store) RequestTask(ctx context.Context, kind string) (Task, bool, error) {
	t, made, err := s.Tasks().Request(ctx, kind)
	return t, made, mapErr(err)
}

// TaskStatus returns the latest request of the kind; see tasks.Store.Status.
func (s *Store) TaskStatus(ctx context.Context, kind string) (Task, error) {
	t, err := s.Tasks().Status(ctx, kind)
	return t, mapErr(err)
}
