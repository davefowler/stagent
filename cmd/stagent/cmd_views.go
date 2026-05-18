package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/davefowler/stagent/internal/events"
	"github.com/spf13/cobra"
)

// cmd_views.go: read-only commands that present projections.
//
//   list    — every task, one line each
//   status  — list + runner-liveness banner
//   show    — detailed view of one task
//   log     — full event log for one task
//   session — print the claude session UUID for (task, role)

func cmdList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()
			tasks, err := store.Tasks(context.Background())
			if err != nil {
				return err
			}
			printTaskTable(cmd.OutOrStdout(), tasks)
			return nil
		},
	}
}

func cmdStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List tasks plus runner liveness",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			pidPath := filepath.Join(wd, ".stagent", "runner.pid")
			alive, pid := pidAlive(pidPath)
			if alive {
				fmt.Fprintf(cmd.OutOrStdout(), "runner: alive (pid %d)\n", pid)
			} else if pid > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "runner: stale pid file (pid %d not running)\n", pid)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "runner: not running")
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()
			tasks, err := store.Tasks(context.Background())
			if err != nil {
				return err
			}
			printTaskTable(cmd.OutOrStdout(), tasks)
			return nil
		},
	}
}

func cmdShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Detailed view of one task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("task id must be an integer: %w", err)
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
			t, err := store.Task(context.Background(), id)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Task #%d: %s\n", t.ID, t.Title)
			fmt.Fprintf(out, "  status:        %s\n", t.Status)
			fmt.Fprintf(out, "  flow:          %s\n", t.Flow)
			fmt.Fprintf(out, "  current_stage: %s\n", t.CurrentStage)
			fmt.Fprintf(out, "  task_file:     %s\n", t.TaskFile)
			fmt.Fprintf(out, "  worktree:      %s\n", t.WorktreeDir)
			fmt.Fprintf(out, "  branch:        %s\n", t.Branch)
			fmt.Fprintf(out, "  created:       %s\n", t.CreatedAt.Format("2006-01-02 15:04:05Z"))
			fmt.Fprintf(out, "  updated:       %s\n", t.UpdatedAt.Format("2006-01-02 15:04:05Z"))

			progress, err := store.StageProgressForTask(context.Background(), id)
			if err == nil && len(progress) > 0 {
				fmt.Fprintln(out, "\nStage progress:")
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  STAGE\tATTEMPTS\tSTATUS")
				for _, p := range progress {
					fmt.Fprintf(tw, "  %s\t%d\t%s\n", p.Stage, p.Attempts, p.Status)
				}
				tw.Flush()
			}

			sessions, err := store.SessionsForTask(context.Background(), id)
			if err == nil && len(sessions) > 0 {
				fmt.Fprintln(out, "\nSessions:")
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "  ROLE\tCLAUDE_ID\tLAST_STAGE\tENDED")
				for _, s := range sessions {
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%v\n", s.Role, s.ClaudeID, s.LastStage, s.Ended)
				}
				tw.Flush()
			}
			return nil
		},
	}
}

func cmdLog() *cobra.Command {
	return &cobra.Command{
		Use:   "log <id>",
		Short: "Print the event log for one task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("task id must be an integer: %w", err)
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
			evs, err := store.ReadByTask(context.Background(), id)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTIME\tTYPE\tSTAGE\tROLE\tACTOR\tPAYLOAD")
			for _, e := range evs {
				payload := string(e.Payload)
				if len(payload) > 80 {
					payload = payload[:77] + "..."
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.ID,
					e.CreatedAt.Format("15:04:05.000"),
					e.Type,
					e.Stage,
					e.Role,
					e.Actor,
					payload)
			}
			return tw.Flush()
		},
	}
}

func cmdSession() *cobra.Command {
	return &cobra.Command{
		Use:   "session <id> <role>",
		Short: "Print the claude session UUID for (task, role)",
		Args:  cobra.ExactArgs(2),
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
			sess, err := store.Session(context.Background(), id, args[1])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), sess.ClaudeID)
			return nil
		},
	}
}

// ─── shared helpers ──────────────────────────────────────────────────

func printTaskTable(out interface{ Write(p []byte) (int, error) }, tasks []events.Task) {
	if len(tasks) == 0 {
		fmt.Fprintln(out, "(no tasks)")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tFLOW\tSTAGE\tTITLE")
	for _, t := range tasks {
		title := t.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			t.ID, t.Status, t.Flow, t.CurrentStage, title)
	}
	tw.Flush()
}

func pidAlive(path string) (alive bool, pid int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return false, 0
	}
	if err := syscall.Kill(n, 0); err == nil {
		return true, n
	}
	return false, n
}
