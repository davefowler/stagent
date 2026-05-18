package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
	"github.com/davefowler/stagent/internal/hooks"
	"github.com/spf13/cobra"
)

func cmdNew() *cobra.Command {
	var flow string
	cmd := &cobra.Command{
		Use:   "new <title-or-path>",
		Short: "Register a new task (from title + template, or an existing file)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			titleOrPath := strings.Join(args, " ")
			useFlow := flow
			if useFlow == "" {
				useFlow = "default"
			}
			return doNew(store, cfg, wd, titleOrPath, useFlow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&flow, "flow", "", "flow name (default: default)")
	return cmd
}

func doNew(store *events.Store, cfg *config.Config, workingDir, titleOrPath, flow string, out interface{ Write(p []byte) (int, error) }) error {
	if _, ok := cfg.Flows[flow]; !ok {
		return fmt.Errorf("unknown flow %q", flow)
	}

	// Allocate next ID.
	nextID, err := nextTaskID(store)
	if err != nil {
		return err
	}

	// Decide title + source file.
	title, srcPath, srcContent, err := resolveSource(workingDir, cfg, titleOrPath, nextID)
	if err != nil {
		return err
	}

	slug := slugify(title)
	taskFileRel := filepath.Join(cfg.TasksDir, fmt.Sprintf("%03d-%s.md", nextID, slug))
	taskFileAbs := filepath.Join(workingDir, taskFileRel)
	if err := os.MkdirAll(filepath.Dir(taskFileAbs), 0o755); err != nil {
		return err
	}

	// Write or move into place.
	if srcPath == "" {
		// title-based: render template
		if err := os.WriteFile(taskFileAbs, srcContent, 0o644); err != nil {
			return err
		}
	} else {
		// path-based: move
		if err := os.Rename(srcPath, taskFileAbs); err != nil {
			// Same-device requirement might fail for cross-filesystem
			// — fall back to copy.
			if err := os.WriteFile(taskFileAbs, srcContent, 0o644); err != nil {
				return err
			}
			_ = os.Remove(srcPath)
		}
	}

	// Validate the task file via the validate_task_sections hook.
	if err := validateTaskFile(cfg, taskFileAbs, taskFileRel, workingDir, flow); err != nil {
		// Roll back: delete the file we just wrote so the user can
		// fix and re-run `stagent new`.
		_ = os.Remove(taskFileAbs)
		return fmt.Errorf("task validation failed:\n%v", err)
	}

	// Emit task.created.
	branch := fmt.Sprintf("task-%03d", nextID)
	worktree := filepath.Join(workingDir, ".worktrees", branch)

	if err := store.AppendPayload(context.Background(), &events.Event{
		TaskID: nextID, Type: events.EventTaskCreated, Actor: events.ActorHuman,
	}, events.TaskCreatedPayload{
		Title:       title,
		Flow:        flow,
		TaskFile:    taskFileRel,
		WorktreeDir: worktree,
		Branch:      branch,
	}); err != nil {
		return err
	}

	fmt.Fprintf(out, "created task #%d: %s\n  file:     %s\n  flow:     %s\n  branch:   %s\n  worktree: %s\n",
		nextID, title, taskFileRel, flow, branch, worktree)
	return nil
}

// resolveSource decides whether titleOrPath is a path (file exists)
// or a title (no file). For title, renders the template at
// .stagent/templates/task.md with {{.Task.Title}} etc. For path,
// returns the file contents (file is later moved into tasks/).
func resolveSource(workingDir string, cfg *config.Config, titleOrPath string, id int64) (title string, srcPath string, content []byte, err error) {
	abs := titleOrPath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workingDir, titleOrPath)
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		b, readErr := os.ReadFile(abs)
		if readErr != nil {
			return "", "", nil, readErr
		}
		// Title from the first H1 line, or filename if none.
		title = firstH1(b)
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
		}
		return title, abs, b, nil
	}

	// Title-based: render template.
	tmplPath := filepath.Join(workingDir, ".stagent", "templates", "task.md")
	rawTmpl, readErr := os.ReadFile(tmplPath)
	if readErr != nil {
		return "", "", nil, fmt.Errorf("read task template %s: %w", tmplPath, readErr)
	}
	tmpl, parseErr := template.New("task").Parse(string(rawTmpl))
	if parseErr != nil {
		return "", "", nil, fmt.Errorf("parse task template: %w", parseErr)
	}
	var buf bytes.Buffer
	data := struct {
		Task struct {
			ID    int64
			Title string
		}
	}{}
	data.Task.ID = id
	data.Task.Title = titleOrPath
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", "", nil, fmt.Errorf("render task template: %w", err)
	}
	return titleOrPath, "", buf.Bytes(), nil
}

// nextTaskID returns max(task_id from task.created) + 1.
func nextTaskID(store *events.Store) (int64, error) {
	tasks, err := store.Tasks(context.Background())
	if err != nil {
		return 0, err
	}
	var max int64
	for _, t := range tasks {
		if t.ID > max {
			max = t.ID
		}
	}
	return max + 1, nil
}

// validateTaskFile runs the validate_task_sections hook against the
// task file. Returns nil on Pass, the failure message on Fail.
func validateTaskFile(cfg *config.Config, taskFileAbs, taskFileRel, workingDir, flow string) error {
	b, err := os.ReadFile(taskFileAbs)
	if err != nil {
		return err
	}
	reg := hooks.NewDefault()
	h, err := reg.Build(config.HookSpec{
		Name: "validate_task_sections",
		Args: map[string]any{},
	})
	if err != nil {
		return err
	}
	res := h.Run(context.Background(), &hooks.Ctx{
		TaskFile:     b,
		TaskFilePath: taskFileRel,
		Config:       cfg,
		WorkingDir:   workingDir,
		Task:         events.Task{Flow: flow},
	})
	if res.Verdict != hooks.Pass {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "task"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func firstH1(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(l[2:])
		}
	}
	return ""
}
