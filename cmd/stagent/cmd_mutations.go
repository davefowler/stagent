package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"syscall"

	"github.com/davefowler/stagent/internal/events"
	"github.com/spf13/cobra"
)

// cmd_mutations.go: commands that append events.

func cmdAbort() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "abort <id>",
		Short: "Abort a task (emits task.aborted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.AppendPayload(context.Background(), &events.Event{
				TaskID: id, Type: events.EventTaskAborted, Actor: events.ActorHuman,
			}, events.TaskAbortedPayload{Reason: reason}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "task #%d aborted\n", id)
			return nil
		},
	}
	cmd.Flags().StringVarP(&reason, "reason", "r", "user_request", "reason recorded in task.aborted payload")
	return cmd
}

func cmdGoto() *cobra.Command {
	var (
		message string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "goto <id> <stage>",
		Short: "Route a task to a chosen stage (human_goto)",
		Long: `Emits stage.entered with reason=human_goto. Counts against
max_runs unless --force is given (also records budget_override).

If -m is provided, the message is recorded so the agent's resume
prompt for the target stage prefixes it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			stage := args[1]

			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if _, ok := cfg.Stages[stage]; !ok {
				return fmt.Errorf("stage %q is not defined in config", stage)
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()

			// Record the message as a hook.fired so the agent's
			// resume prompt picks it up. Matches runner/handleRedirect.
			if message != "" {
				if err := store.AppendPayload(context.Background(), &events.Event{
					TaskID: id, Type: events.EventHookFired, Stage: stage,
					Actor: events.ActorHuman,
				}, events.HookFiredPayload{
					Hook:    "goto",
					Result:  "redirect",
					Message: message,
				}); err != nil {
					return err
				}
			}

			role := cfg.Stages[stage].Role
			payload := events.StageEnteredPayload{
				Reason:         events.ReasonHumanGoto,
				StageType:      events.StageType(cfg.Stages[stage].Type),
				BudgetOverride: force,
			}
			attempts, err := store.StageAttempts(context.Background(), id, stage)
			if err != nil {
				return err
			}
			payload.Attempt = attempts + 1

			if !force {
				if attempts >= cfg.Stages[stage].MaxRuns {
					return fmt.Errorf("stage %q max_runs (%d) reached; use --force to override",
						stage, cfg.Stages[stage].MaxRuns)
				}
			}

			if err := store.AppendPayload(context.Background(), &events.Event{
				TaskID: id, Type: events.EventStageEntered, Stage: stage,
				Role: role, Actor: events.ActorHuman,
			}, payload); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "task #%d goto %s (attempt %d, force=%v)\n",
				id, stage, payload.Attempt, force)
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "",
		"message prepended to the agent's resume prompt on the target stage")
	cmd.Flags().BoolVar(&force, "force", false,
		"bypass max_runs (records budget_override in the stage.entered payload)")
	return cmd
}

func cmdRestart() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <id>",
		Short: "Restart the current stage as a retry (kills the active session)",
		Long: `Looks up the active session for the task's current stage,
sends SIGTERM if one is alive, then emits stage.entered with
reason=retry so the worker re-invokes claude with --resume.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()

			t, err := store.Task(context.Background(), id)
			if err != nil {
				return err
			}
			stage := t.CurrentStage
			if stage == "" {
				return fmt.Errorf("task #%d has no current stage", id)
			}

			// Find the most recent session for the stage's role and
			// kill its PID if it's still alive.
			role := cfg.Stages[stage].Role
			if role != "" {
				if pid, err := pidFromLastSession(store, id, role); err == nil && pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGTERM)
					fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to pid %d\n", pid)
				}
			}

			// Emit stage.entered with reason=retry.
			attempts, err := store.StageAttempts(context.Background(), id, stage)
			if err != nil {
				return err
			}
			return store.AppendPayload(context.Background(), &events.Event{
				TaskID: id, Type: events.EventStageEntered, Stage: stage,
				Role: role, Actor: events.ActorHuman,
			}, events.StageEnteredPayload{
				Attempt:   attempts + 1,
				Reason:    events.ReasonRetry,
				StageType: events.StageType(cfg.Stages[stage].Type),
			})
		},
	}
}

// pidFromLastSession scans the event log backward for the latest
// session.started/resumed on (task, role) and returns its PID.
func pidFromLastSession(store *events.Store, taskID int64, role string) (int, error) {
	evs, err := store.ReadByTask(context.Background(), taskID)
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
		return pl.PID, nil
	}
	return 0, nil
}
