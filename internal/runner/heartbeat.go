package runner

import (
	"context"
	"time"

	"github.com/davefowler/stagent/internal/events"
)

// heartbeat is the runner's main loop. It scans the task projection
// at each tick and spawns a worker for any active task that doesn't
// already have one. Workers self-terminate when their task is
// terminal (completed / failed / aborted) and remove themselves
// from the map.
func (r *Runner) heartbeat(ctx context.Context) {
	interval := r.HeartbeatInterval()
	t := time.NewTicker(interval)
	defer t.Stop()

	// Tick immediately on start so a fresh `stagent run` doesn't
	// idle for one interval before picking up existing tasks.
	r.dispatch(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			r.dispatch(ctx)
		}
	}
}

// dispatch finds active tasks and ensures each has a running worker.
func (r *Runner) dispatch(ctx context.Context) {
	tasks, err := r.opts.Store.Tasks(ctx)
	if err != nil {
		r.opts.Logger.Error("heartbeat: list tasks", "err", err)
		return
	}
	for _, t := range tasks {
		if isTerminal(t.Status) {
			continue
		}
		if _, exists := r.workers.LoadOrStore(t.ID, "pending"); exists {
			continue
		}
		w := newWorker(r, t)
		r.workers.Store(t.ID, w)
		r.workerWG.Add(1)
		go func(taskID int64) {
			defer r.workerWG.Done()
			defer r.workers.Delete(taskID)
			w.Run(ctx)
		}(t.ID)
	}
}

// isTerminal returns true for task statuses the runner should leave
// alone. `waiting_human` is NOT terminal in v0.2; in v0.1 the
// validator rejects human stages so this branch is dead.
func isTerminal(s events.TaskStatus) bool {
	switch s {
	case events.TaskStatusCompleted, events.TaskStatusFailed, events.TaskStatusAborted:
		return true
	default:
		return false
	}
}
