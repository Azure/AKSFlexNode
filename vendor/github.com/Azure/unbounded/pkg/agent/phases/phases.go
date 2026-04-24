// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package phases

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// Task is the interface implemented by all phase tasks.
type Task interface {
	Name() string
	Do(ctx context.Context) error
}

// ExecuteTask logs start, then runs t.Do, then logs completion or failure in the
// "wide" format: task name, start time, status and elapsed duration.
// Any panic from t.Do is recovered, converted to an error, and logged as a failure.
func ExecuteTask(ctx context.Context, log *slog.Logger, t Task) (err error) {
	name := t.Name()
	start := time.Now()

	log.Info("started", slog.String("task", name))

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}

		elapsed := time.Since(start)

		if err != nil {
			log.Error("failed",
				slog.String("task", name),
				slog.Duration("duration", elapsed),
				slog.String("status", "failed"),
				slog.String("error", err.Error()),
			)
		} else {
			log.Info("completed",
				slog.String("task", name),
				slog.Duration("duration", elapsed),
				slog.String("status", "ok"),
			)
		}
	}()

	err = t.Do(ctx)

	return err
}

// Serial returns a Task that executes the given tasks one by one in order,
// stopping and returning the first error encountered.
// The provided logger is used to emit a wide log line for each task.
func Serial(log *slog.Logger, tasks ...Task) Task {
	return &serial{log: log, tasks: tasks}
}

type serial struct {
	log   *slog.Logger
	tasks []Task
}

func (s *serial) Name() string { return taskGroupName("serial", s.tasks) }

func (s *serial) Do(ctx context.Context) error {
	for _, t := range s.tasks {
		if err := ExecuteTask(ctx, s.log, t); err != nil {
			return fmt.Errorf("%s: %w", t.Name(), err)
		}
	}

	return nil
}

// Parallel returns a Task that executes the given tasks concurrently.
// All tasks are started at once. On the first error the context passed to
// remaining tasks is cancelled and the first error is returned.
// The provided logger is used to emit a wide log line for each task.
func Parallel(log *slog.Logger, tasks ...Task) Task {
	return &parallel{log: log, tasks: tasks}
}

type parallel struct {
	log   *slog.Logger
	tasks []Task
}

func (p *parallel) Name() string { return taskGroupName("parallel", p.tasks) }

func (p *parallel) Do(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)

	for _, t := range p.tasks {
		eg.Go(func() error {
			if err := ExecuteTask(ctx, p.log, t); err != nil {
				return fmt.Errorf("%s: %w", t.Name(), err)
			}

			return nil
		})
	}

	return eg.Wait()
}

// taskGroupName builds a display name for a group of tasks in the form
// "kind(name1, name2, ...)".
func taskGroupName(kind string, tasks []Task) string {
	var b strings.Builder

	b.WriteString(kind)
	b.WriteByte('(')

	for i, t := range tasks {
		if i > 0 {
			b.WriteString(", ")
		}

		b.WriteString(t.Name())
	}

	b.WriteByte(')')

	return b.String()
}
