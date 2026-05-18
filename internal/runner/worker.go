package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
	"github.com/davefowler/stagent/internal/hooks"
)

// worker owns one task's state machine. Its public surface is just
// Run; everything else is internal.
type worker struct {
	r      *Runner
	taskID int64
	log    *slog.Logger
}

func newWorker(r *Runner, t events.Task) *worker {
	return &worker{
		r:      r,
		taskID: t.ID,
		log:    r.opts.Logger.With("task_id", t.ID, "task", t.Title),
	}
}

// Run drives the task to a terminal state. It steps in a loop:
// look at the event log, decide the next action, do it, repeat.
// Returns when the task is completed/failed/aborted, when ctx is
// cancelled, or on an unrecoverable error.
//
// One step per loop iteration so SIGHUP reloads, ctx cancellation,
// and inter-worker interleaving all happen at safe points.
func (w *worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		task, err := w.r.opts.Store.Task(ctx, w.taskID)
		if errors.Is(err, events.ErrNotFound) {
			w.log.Error("task projection vanished; stopping worker")
			return
		} else if err != nil {
			w.log.Error("worker: read task", "err", err)
			w.sleep(ctx, 500*time.Millisecond)
			continue
		}

		if isTerminal(task.Status) {
			w.log.Info("task terminal", "status", task.Status)
			return
		}

		done, err := w.step(ctx, task)
		if err != nil {
			w.log.Error("worker step error", "err", err)
			w.sleep(ctx, 500*time.Millisecond)
			continue
		}
		if done {
			return
		}
	}
}

// step performs one unit of work. Returns done=true if the worker
// should exit. Otherwise the loop iterates and re-derives state
// from the event log.
func (w *worker) step(ctx context.Context, task events.Task) (bool, error) {
	rt := w.r.current()
	cfg := rt.cfg

	flow, ok := cfg.Flows[task.Flow]
	if !ok {
		// Unknown flow → fail the task by emitting stage.failed on a
		// synthetic stage. Easier: log + sleep so a SIGHUP reload
		// with a corrected config can pick it up. For v0.1 we just
		// log and idle; the user can `stagent abort` it.
		w.log.Error("unknown flow", "flow", task.Flow)
		w.sleep(ctx, 5*time.Second)
		return false, nil
	}

	// Find the current stage from the event log.
	stageName, stageState, err := w.currentStage(ctx, task.ID, flow)
	if err != nil {
		return false, err
	}

	switch stageState {
	case stageStateNotStarted:
		return false, w.enterStage(ctx, task, cfg, stageName, events.ReasonFlow, "")

	case stageStateEntered:
		return false, w.runEnterHooks(ctx, task, cfg, rt, stageName)

	case stageStateRunning:
		return false, w.runAgentOrScript(ctx, task, cfg, rt, stageName)

	case stageStateSessionEnded, stageStateScriptReady:
		return false, w.runExitHooks(ctx, task, cfg, rt, stageName)

	case stageStateCompleted:
		return false, w.advance(ctx, task, flow, stageName)

	case stageStateFailed:
		w.log.Info("stage failed; task terminating", "stage", stageName)
		return true, nil
	}
	return false, fmt.Errorf("unhandled stage state: %v", stageState)
}

// stageState enumerates the per-stage sub-states the worker
// recognises by reading the event log.
type stageState int

const (
	stageStateNotStarted stageState = iota
	stageStateEntered                  // stage.entered emitted, enter hooks not run yet (or were re-entered after a crash)
	stageStateRunning                  // agent process should be running (session.started without session.ended)
	stageStateSessionEnded             // agent exited; exit hooks pending
	stageStateScriptReady              // script stage entered + enter hooks done; ready for exit hooks
	stageStateCompleted
	stageStateFailed
)

// currentStage decides which stage in the flow the task is on right
// now, plus the sub-state for that stage. It does so by reading the
// task's events and looking at the most-recent transitions.
//
// The algorithm:
//  1. Walk events in reverse, find the most recent stage.entered.
//     That's the current stage.
//  2. After that stage.entered, look for stage.completed/failed on
//     the same stage — if found, advance / fail respectively.
//  3. Otherwise, look at session events on the same (stage, role).
//     They tell us where the agent process is.
//  4. For script stages with no session events, we treat the stage
//     as "ready" — enter hooks may need to run; the exit-hook step
//     is idempotent because we re-derive on the next loop iteration.
func (w *worker) currentStage(ctx context.Context, taskID int64, flow config.Flow) (string, stageState, error) {
	evs, err := w.r.opts.Store.ReadByTask(ctx, taskID)
	if err != nil {
		return "", 0, err
	}

	// Find last stage.entered.
	var (
		stageName    string
		enteredIdx   = -1
		enteredEvent events.Event
	)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == events.EventStageEntered {
			enteredIdx = i
			enteredEvent = evs[i]
			stageName = evs[i].Stage
			break
		}
	}
	if enteredIdx < 0 {
		// No stage.entered yet → start with the first stage in the flow.
		if len(flow) == 0 {
			return "", stageStateFailed, fmt.Errorf("flow has zero stages")
		}
		return flow[0], stageStateNotStarted, nil
	}

	// Look at events AFTER the latest stage.entered (on the same task).
	after := evs[enteredIdx+1:]

	for _, e := range after {
		if e.Stage != stageName {
			continue
		}
		switch e.Type {
		case events.EventStageCompleted:
			return stageName, stageStateCompleted, nil
		case events.EventStageFailed:
			return stageName, stageStateFailed, nil
		}
	}

	// No stage-level transition since entry — figure out sub-state.
	var pl events.StageEnteredPayload
	if err := json.Unmarshal(enteredEvent.Payload, &pl); err != nil {
		return stageName, 0, fmt.Errorf("decode stage.entered payload: %w", err)
	}

	if pl.StageType == events.StageScript {
		return stageName, stageStateScriptReady, nil
	}

	// Agent stage. Inspect session events after stage.entered.
	var (
		sessionStarted bool
		sessionEnded   bool
	)
	for _, e := range after {
		if e.Stage != stageName {
			continue
		}
		switch e.Type {
		case events.EventSessionStarted, events.EventSessionResumed:
			sessionStarted = true
			sessionEnded = false
		case events.EventSessionEnded:
			sessionEnded = true
		}
	}
	switch {
	case !sessionStarted:
		return stageName, stageStateEntered, nil
	case sessionStarted && !sessionEnded:
		return stageName, stageStateRunning, nil
	default:
		return stageName, stageStateSessionEnded, nil
	}
}

// ─── Transition implementations ─────────────────────────────────────

// enterStage emits stage.entered for stageName with the given reason.
// Honors max_runs: if the stage has been entered too many times,
// emits stage.failed instead.
func (w *worker) enterStage(ctx context.Context, task events.Task, cfg *config.Config, stageName string, reason events.EnterReason, fromStage string) error {
	stageDef, ok := cfg.Stages[stageName]
	if !ok {
		return fmt.Errorf("enter: stage %q not defined in config", stageName)
	}

	attempts, err := w.r.opts.Store.StageAttempts(ctx, task.ID, stageName)
	if err != nil {
		return err
	}
	if attempts >= stageDef.MaxRuns {
		w.log.Warn("max_runs exhausted; emitting stage.failed",
			"stage", stageName, "attempts", attempts, "max_runs", stageDef.MaxRuns)
		return w.appendEvent(ctx, &events.Event{
			TaskID: task.ID, Type: events.EventStageFailed, Stage: stageName,
			Actor: events.ActorHeartbeat,
		}, events.StageFailedPayload{Reason: "budget_exhausted",
			LastError: fmt.Sprintf("max_runs=%d reached", stageDef.MaxRuns)})
	}

	payload := events.StageEnteredPayload{
		Attempt:   attempts + 1,
		Reason:    reason,
		StageType: events.StageType(stageDef.Type),
		FromStage: fromStage,
	}
	role := ""
	if stageDef.Type == config.StageAgent {
		role = stageDef.Role
	}
	w.log.Info("stage.entered", "stage", stageName, "attempt", payload.Attempt, "reason", reason)
	return w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventStageEntered, Stage: stageName,
		Role: role, Actor: events.ActorHeartbeat,
	}, payload)
}

// runEnterHooks executes enter hooks for an agent stage. (Script
// stages bypass this path because currentStage() reports them as
// "scriptReady" immediately — enter+exit hooks run together in
// runExitHooks.)
//
// On Pass: returns nil; next loop iteration will start the agent.
// On Fail: emits stage.failed (or schedules a retry by emitting
// another stage.entered with reason=retry).
func (w *worker) runEnterHooks(ctx context.Context, task events.Task, cfg *config.Config, rt *runtime, stageName string) error {
	stageDef := cfg.Stages[stageName]
	enterHooks := rt.stageHooks[stageHookKey{stageName, slotEnter}]
	if len(enterHooks) == 0 {
		// Nothing to do — start the agent immediately on next step
		// by emitting a synthetic session-not-started condition. We
		// simulate this by just falling through: the next call to
		// currentStage will still see stageStateEntered (no
		// session). Without enter hooks to gate, runAgentOrScript
		// is what advances. We need a way to skip enter hooks.
		// Simplest: directly start the agent here.
		return w.startOrResumeAgent(ctx, task, cfg, rt, stageName)
	}

	hctx, err := w.buildHookCtx(task, stageDef.Role, stageName, cfg)
	if err != nil {
		return err
	}

	for _, h := range enterHooks {
		result := h.Run(ctx, hctx)
		w.recordHookFired(ctx, task, stageName, h, result)
		switch result.Verdict {
		case hooks.Pass:
			continue
		case hooks.Fail:
			return w.handleHookFail(ctx, task, cfg, stageName, "enter", h.Name(), result.Message)
		case hooks.Redirect:
			return w.handleRedirect(ctx, task, cfg, stageName, result.Target, result.Message)
		default:
			return fmt.Errorf("enter hook %q returned unexpected verdict %v", h.Name(), result.Verdict)
		}
	}

	// All enter hooks passed. For agent stages, start the subprocess.
	return w.startOrResumeAgent(ctx, task, cfg, rt, stageName)
}

// runAgentOrScript dispatches based on stage type for the
// "stageStateRunning" sub-state — meaning a session was started
// and hasn't ended yet. For agent stages we wait via claude.go's
// process tracking. For script stages this state is unreachable
// (no session events).
func (w *worker) runAgentOrScript(ctx context.Context, task events.Task, cfg *config.Config, rt *runtime, stageName string) error {
	stageDef := cfg.Stages[stageName]
	if stageDef.Type == config.StageScript {
		// Shouldn't happen — script stages don't emit session
		// events. If we got here, the event log is in a weird
		// shape; treat the stage as ready for exit hooks.
		return w.runExitHooks(ctx, task, cfg, rt, stageName)
	}
	// Agent stage. The process is being awaited by waitForAgent
	// when it was spawned in startOrResumeAgent; if we arrived here
	// fresh (e.g., after a crash + recovery emitted session.ended
	// already), the next state will be stageStateSessionEnded.
	// Just sleep briefly so we don't spin.
	w.sleep(ctx, 200*time.Millisecond)
	return nil
}

// runExitHooks runs exit hooks for the current stage. On Pass:
// emits stage.completed. On Fail: max_runs check → retry or fail.
// On Redirect: emits stage.completed + stage.entered(target).
func (w *worker) runExitHooks(ctx context.Context, task events.Task, cfg *config.Config, rt *runtime, stageName string) error {
	stageDef := cfg.Stages[stageName]
	exitHooks := rt.stageHooks[stageHookKey{stageName, slotExit}]
	hctx, err := w.buildHookCtx(task, stageDef.Role, stageName, cfg)
	if err != nil {
		return err
	}

	// For script stages, run enter hooks first (we collapsed enter+exit
	// into the script flow for v0.1 since there's no "in between" agent
	// session to wait on).
	if stageDef.Type == config.StageScript {
		enterHooks := rt.stageHooks[stageHookKey{stageName, slotEnter}]
		for _, h := range enterHooks {
			result := h.Run(ctx, hctx)
			w.recordHookFired(ctx, task, stageName, h, result)
			switch result.Verdict {
			case hooks.Pass:
				continue
			case hooks.Fail:
				return w.handleHookFail(ctx, task, cfg, stageName, "enter", h.Name(), result.Message)
			case hooks.Redirect:
				return w.handleRedirect(ctx, task, cfg, stageName, result.Target, result.Message)
			default:
				return fmt.Errorf("enter hook %q returned unexpected verdict %v", h.Name(), result.Verdict)
			}
		}
	}

	for _, h := range exitHooks {
		result := h.Run(ctx, hctx)
		w.recordHookFired(ctx, task, stageName, h, result)
		switch result.Verdict {
		case hooks.Pass:
			continue
		case hooks.Fail:
			return w.handleHookFail(ctx, task, cfg, stageName, "exit", h.Name(), result.Message)
		case hooks.Redirect:
			return w.handleRedirect(ctx, task, cfg, stageName, result.Target, result.Message)
		default:
			return fmt.Errorf("exit hook %q returned unexpected verdict %v", h.Name(), result.Verdict)
		}
	}

	// All hooks passed. Emit stage.completed.
	w.log.Info("stage.completed", "stage", stageName)
	return w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventStageCompleted, Stage: stageName,
		Actor: events.ActorHeartbeat,
	}, struct{}{})
}

// advance emits stage.entered for the next stage in flow, or
// task.completed if the current stage was last.
func (w *worker) advance(ctx context.Context, task events.Task, flow config.Flow, currentStage string) error {
	idx := -1
	for i, s := range flow {
		if s == currentStage {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("advance: stage %q not in flow %v", currentStage, flow)
	}
	if idx == len(flow)-1 {
		w.log.Info("task.completed")
		return w.appendEvent(ctx, &events.Event{
			TaskID: task.ID, Type: events.EventTaskCompleted,
			Actor: events.ActorHeartbeat,
		}, struct{}{})
	}
	next := flow[idx+1]
	return w.enterStage(ctx, task, w.r.current().cfg, next, events.ReasonFlow, currentStage)
}

// handleHookFail decides retry vs fail. A retry emits a new
// stage.entered with reason=retry so the worker re-enters the
// same stage on the next loop. Fail emits stage.failed and the
// worker stops.
func (w *worker) handleHookFail(ctx context.Context, task events.Task, cfg *config.Config, stageName, slot, hookName, msg string) error {
	w.log.Warn("hook failed", "stage", stageName, "slot", slot, "hook", hookName, "msg", msg)
	stageDef := cfg.Stages[stageName]
	attempts, err := w.r.opts.Store.StageAttempts(ctx, task.ID, stageName)
	if err != nil {
		return err
	}
	if attempts >= stageDef.MaxRuns {
		return w.appendEvent(ctx, &events.Event{
			TaskID: task.ID, Type: events.EventStageFailed, Stage: stageName,
			Actor: events.ActorHeartbeat,
		}, events.StageFailedPayload{Reason: hookName + " failed", LastError: msg})
	}
	// Retry. The redirect-message-style hook hint goes into the
	// resume prompt via the agent loader — we stash it in the
	// stage.entered payload's from_stage? No, that's for redirects.
	// For retries, the agent's next prompt should include the hook
	// failure message. We bake it into the resume prompt later via
	// recent hook.fired event(s); see startOrResumeAgent.
	return w.enterStage(ctx, task, cfg, stageName, events.ReasonRetry, "")
}

// handleRedirect emits stage.completed on the current stage (the
// work was done — reviewer reached a verdict) and stage.entered
// on the target with reason=redirect. The redirect message is
// stashed in the target's payload via FromStage + a recent
// hook.fired event the agent loader picks up.
func (w *worker) handleRedirect(ctx context.Context, task events.Task, cfg *config.Config, fromStage, targetStage, msg string) error {
	w.log.Info("redirect", "from", fromStage, "to", targetStage)
	if _, ok := cfg.Stages[targetStage]; !ok {
		return fmt.Errorf("redirect target %q is not defined", targetStage)
	}
	if err := w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventStageCompleted, Stage: fromStage,
		Actor: events.ActorHeartbeat,
	}, struct{}{}); err != nil {
		return err
	}
	// Record the redirect message as a hook.fired event so the
	// agent loader can prepend it to the resume prompt.
	if err := w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventHookFired, Stage: targetStage,
		Actor: events.ActorSystem,
	}, events.HookFiredPayload{Hook: "redirect", Result: "redirect", Message: msg}); err != nil {
		return err
	}
	return w.enterStage(ctx, task, cfg, targetStage, events.ReasonRedirect, fromStage)
}

// recordHookFired emits a hook.fired event for observability.
func (w *worker) recordHookFired(ctx context.Context, task events.Task, stage string, h hooks.Hook, result hooks.Result) {
	_ = w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventHookFired, Stage: stage,
		Actor: events.ActorHeartbeat,
	}, events.HookFiredPayload{
		Hook:    h.Name(),
		Result:  result.Verdict.String(),
		Message: result.Message,
	})
}

// buildHookCtx reads the task file once and assembles a Ctx the
// hooks operate on.
func (w *worker) buildHookCtx(task events.Task, role, stage string, cfg *config.Config) (*hooks.Ctx, error) {
	src := []byte{}
	if task.TaskFile != "" {
		path := task.TaskFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(w.r.opts.WorkingDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read task file %q: %w", path, err)
		}
		src = b
	}
	return &hooks.Ctx{
		Task:         task,
		Stage:        stage,
		Role:         role,
		TaskFile:     src,
		TaskFilePath: task.TaskFile,
		Config:       cfg,
		WorkingDir:   w.r.opts.WorkingDir,
	}, nil
}

// appendEvent is a small wrapper that JSON-encodes the payload.
func (w *worker) appendEvent(ctx context.Context, e *events.Event, payload any) error {
	if err := w.r.opts.Store.AppendPayload(ctx, e, payload); err != nil {
		return fmt.Errorf("append %s: %w", e.Type, err)
	}
	return nil
}

func (w *worker) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
