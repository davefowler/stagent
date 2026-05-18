package runner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/davefowler/stagent/internal/events"
)

// recover scans for orphaned sessions left by a previous runner
// process and emits session.ended events for them so workers can
// pick up where they left off (or retry, depending on hook
// outcomes). See notes/architecture.md "Crash recovery".
//
// For each active session (no matching session.ended), we check
// whether the recorded PID is still alive. If yes, it's a true
// orphan: the runner died but the child kept running. We emit
// session.ended with reason="runner_restart_orphan" and let the
// retry budget handle the next step. If no, the process is gone
// already, same handling.
//
// We do NOT try to reattach to surviving children — they'd be PID
// 1's now, no `Wait()` handle, and we can't reliably observe their
// exit. Better to record them as ended and let `--resume` pick up
// where their JSONL transcript left off.
func (r *Runner) recover(ctx context.Context) error {
	active, err := r.opts.Store.ActiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("active sessions: %w", err)
	}
	for _, sess := range active {
		pid, err := pidForSession(ctx, r.opts.Store, sess.TaskID, sess.Role, sess.ClaudeID)
		if err != nil {
			r.opts.Logger.Warn("recovery: cannot determine PID", "task_id", sess.TaskID, "role", sess.Role, "err", err)
			pid = 0
		}
		r.opts.Logger.Info("recovery: reaping orphan session",
			"task_id", sess.TaskID,
			"role", sess.Role,
			"session_id", sess.ClaudeID,
			"recorded_pid", pid,
			"still_alive", pid > 0 && isAlive(pid))

		// Note: if the process IS still alive we leave it running.
		// Real Claude in --resume mode appends to the same JSONL,
		// so when the worker eventually invokes --resume the new
		// turn picks up after the orphan's last line. If two
		// processes are writing concurrently, JSONL append is
		// well-ordered (O_APPEND), so the worst case is a slightly
		// interleaved transcript.

		if err := r.opts.Store.AppendPayload(ctx, &events.Event{
			TaskID: sess.TaskID,
			Type:   events.EventSessionEnded,
			Stage:  sess.LastStage,
			Role:   sess.Role,
			Actor:  events.ActorSystem,
		}, events.SessionEndedPayload{
			Reason:   "runner_restart_orphan",
			ExitCode: -1,
		}); err != nil {
			return fmt.Errorf("append session.ended for orphan: %w", err)
		}
	}
	return nil
}

// pidForSession looks up the PID recorded with the most recent
// session.started or session.resumed for (taskID, role, sessionID).
func pidForSession(ctx context.Context, store *events.Store, taskID int64, role, sessionID string) (int, error) {
	evs, err := store.ReadByTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Role != role {
			continue
		}
		if e.Type != events.EventSessionStarted && e.Type != events.EventSessionResumed {
			continue
		}
		var pl events.SessionStartedPayload
		if err := json.Unmarshal(e.Payload, &pl); err != nil {
			continue
		}
		if pl.ClaudeSessionID == sessionID {
			return pl.PID, nil
		}
	}
	return 0, nil
}
