package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"

	"github.com/google/uuid"
)

// startOrResumeAgent invokes the Claude binary for the current
// stage. First invocation for (task, role): generate UUID, pass
// --session-id, emit session.started. Subsequent invocations:
// pass --resume <uuid>, emit session.resumed.
//
// The call blocks until the subprocess exits. On exit, emits
// session.ended with the reason and exit code. The next loop
// iteration sees stageStateSessionEnded and runs exit hooks.
func (w *worker) startOrResumeAgent(ctx context.Context, task events.Task, cfg *config.Config, rt *runtime, stageName string) error {
	stageDef := cfg.Stages[stageName]
	if stageDef.Type != config.StageAgent {
		// Script stage with no enter hooks falls through here.
		// Treat as ready for exit hooks.
		return w.runExitHooks(ctx, task, cfg, rt, stageName)
	}

	role := stageDef.Role
	sess, err := w.r.opts.Store.Session(ctx, task.ID, role)
	hasPrior := err == nil && sess.ClaudeID != ""
	if err != nil && !errors.Is(err, events.ErrNotFound) {
		return fmt.Errorf("read session: %w", err)
	}

	prompt, err := buildStagePrompt(ctx, w.r.opts.Store, task, rt, stageName)
	if err != nil {
		return err
	}

	worktreeDir := task.WorktreeDir
	if worktreeDir == "" || !dirExists(worktreeDir) {
		worktreeDir = w.r.opts.WorkingDir
	}

	args := []string{"-p", prompt, "--dangerously-skip-permissions"}
	var sessionUUID string
	if hasPrior {
		sessionUUID = sess.ClaudeID
		args = append(args, "--resume", sessionUUID)
	} else {
		sessionUUID = uuid.NewString()
		args = append(args, "--session-id", sessionUUID)
		// First invocation gets the system prompt.
		if sp, ok := rt.rolePrompt[role]; ok && sp != "" {
			// Write the system prompt to a temp file and pass with @.
			// Avoids quoting issues on long markdown prompts.
			tmp, err := os.CreateTemp("", "stagent-sys-*.md")
			if err != nil {
				return fmt.Errorf("create system prompt tmp: %w", err)
			}
			defer os.Remove(tmp.Name())
			if _, err := tmp.WriteString(sp); err != nil {
				return err
			}
			tmp.Close()
			args = append(args, "--system-prompt", "@"+tmp.Name())
		}
	}

	cmd := exec.CommandContext(ctx, w.r.opts.ClaudeBin, args...)
	cmd.Dir = worktreeDir
	cmd.Stdout = os.Stderr // visible in `stagent run` foreground
	cmd.Stderr = os.Stderr

	w.log.Info("claude invoke",
		"stage", stageName,
		"role", role,
		"session_id", sessionUUID,
		"resume", hasPrior,
		"cwd", worktreeDir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude start: %w", err)
	}

	startEvent := events.EventSessionStarted
	if hasPrior {
		startEvent = events.EventSessionResumed
	}
	if err := w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: startEvent, Stage: stageName, Role: role,
		Actor: events.ActorAgent,
	}, events.SessionStartedPayload{
		ClaudeSessionID: sessionUUID,
		PID:             cmd.Process.Pid,
	}); err != nil {
		return err
	}

	waitErr := cmd.Wait()
	exitCode := 0
	endReason := "completed"
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			endReason = "exited"
		} else if errors.Is(ctx.Err(), context.Canceled) {
			endReason = "cancelled"
		} else {
			endReason = "unknown"
		}
	}
	w.log.Info("claude exited", "session_id", sessionUUID, "exit_code", exitCode, "reason", endReason)

	return w.appendEvent(ctx, &events.Event{
		TaskID: task.ID, Type: events.EventSessionEnded, Stage: stageName, Role: role,
		Actor: events.ActorAgent,
	}, events.SessionEndedPayload{Reason: endReason, ExitCode: exitCode})
}

// buildStagePrompt renders the stage prompt template with the task
// context and prepends any pending redirect/retry message recorded
// since the last stage.entered.
func buildStagePrompt(ctx context.Context, store *events.Store, task events.Task, rt *runtime, stageName string) (string, error) {
	raw, ok := rt.stagePrompt[stageName]
	if !ok {
		// No stage prompt → ship a minimal one. The agent will still
		// have the role system prompt + the task file path.
		raw = fmt.Sprintf("Work on stage %q for task %d (%s).\nTask file: {{.Task.TaskFile}}\nWorktree: {{.Task.WorktreeDir}}\n",
			stageName, task.ID, task.Title)
	}

	tmpl, err := template.New("stage").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("stage %q prompt template: %w", stageName, err)
	}

	prefix, err := pendingMessageForStage(ctx, store, task.ID, stageName)
	if err != nil {
		return "", err
	}

	data := struct {
		Task            events.Task
		RedirectMessage string
	}{Task: task, RedirectMessage: prefix}

	var buf bytes.Buffer
	if prefix != "" {
		fmt.Fprintln(&buf, prefix)
		fmt.Fprintln(&buf, strings.Repeat("─", 40))
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("stage %q prompt render: %w", stageName, err)
	}
	return buf.String(), nil
}

// pendingMessageForStage returns the most recent hook.fired Message
// for the current stage attempt — i.e., any hook failure or redirect
// recorded since the latest stage.entered. The agent sees this
// prepended to its prompt so it knows what to fix.
func pendingMessageForStage(ctx context.Context, store *events.Store, taskID int64, stageName string) (string, error) {
	evs, err := store.ReadByTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	// Find the most recent stage.entered for this stage.
	enteredIdx := -1
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == events.EventStageEntered && evs[i].Stage == stageName {
			enteredIdx = i
			break
		}
	}
	if enteredIdx < 0 {
		return "", nil
	}
	// Collect hook.fired messages BEFORE this stage.entered (they're
	// what triggered the retry / redirect into the new entry).
	// We look in the window between the prior stage.entered (or 0)
	// and the current one.
	priorIdx := -1
	for i := enteredIdx - 1; i >= 0; i-- {
		if evs[i].Type == events.EventStageEntered {
			priorIdx = i
			break
		}
	}
	var msgs []string
	for i := priorIdx + 1; i < enteredIdx; i++ {
		e := evs[i]
		if e.Type != events.EventHookFired {
			continue
		}
		var pl events.HookFiredPayload
		if err := json.Unmarshal(e.Payload, &pl); err != nil {
			continue
		}
		if pl.Message != "" && (pl.Result == "fail" || pl.Result == "redirect") {
			msgs = append(msgs, pl.Message)
		}
	}
	return strings.Join(msgs, "\n\n"), nil
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

