package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
	"github.com/davefowler/stagent/internal/hooks"
)

// buildFraude compiles the mock-claude binary into a temp dir and
// returns its path. The runner tests use it as ClaudeBin so they
// don't depend on a real claude install.
func buildFraude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fraude")
	// Resolve repo root from the package dir: internal/runner/ → ../../
	cmd := exec.Command("go", "build", "-o", out, "../../cmd/fraude")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build fraude: %v", err)
	}
	return out
}

// projectFixture lays down a minimal stagent project on disk. The
// flow is a single agent stage that fraude can complete by ticking
// every checkbox in the Implementation plan section.
type projectFixture struct {
	dir          string
	taskFilePath string
	dbPath       string
}

func setupProject(t *testing.T, scriptedResponses []map[string]any) projectFixture {
	t.Helper()
	dir := t.TempDir()

	// .stagent dirs
	for _, sub := range []string{
		filepath.Join(".stagent", "prompts", "roles"),
		filepath.Join(".stagent", "prompts", "stages"),
		"tasks",
	} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Role + stage prompts.
	must(t, os.WriteFile(filepath.Join(dir, ".stagent", "prompts", "roles", "developer.md"),
		[]byte("You are a developer agent. Edit task files to satisfy hooks.\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".stagent", "prompts", "stages", "code.md"),
		[]byte("Task: {{.Task.Title}}\nFile: {{.Task.TaskFile}}\nFinish the implementation plan.\n"), 0o644))

	// Config.
	cfg := strings.TrimSpace(`
roles:
  developer:
    model: opus
    bound: task
stages:
  code:
    type: agent
    role: developer
    hooks:
      exit:
        - section_check:
            section: "Implementation plan"
            expect: all_checked
flows:
  default:
    - code
heartbeat:
  interval: 50ms
`) + "\n"
	must(t, os.WriteFile(filepath.Join(dir, ".stagent.yaml"), []byte(cfg), 0o644))

	// Task file.
	task := strings.TrimSpace(`
# Demo task

## Implementation plan

- [ ] step one
- [ ] step two
`) + "\n"
	taskFile := filepath.Join(dir, "tasks", "001-demo.md")
	must(t, os.WriteFile(taskFile, []byte(task), 0o644))

	// Scripted responses for fraude.
	respPath := filepath.Join(dir, "responses.json")
	b, err := json.MarshalIndent(scriptedResponses, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(respPath, b, 0o644))
	t.Setenv("STAGENT_FAKE_RESPONSES", respPath)

	return projectFixture{
		dir:          dir,
		taskFilePath: filepath.Join("tasks", "001-demo.md"),
		dbPath:       filepath.Join(dir, ".stagent", "stagent.db"),
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCompletesAgentTask(t *testing.T) {
	fraudeBin := buildFraude(t)

	// One scripted turn that ticks both boxes, then exits 0.
	responses := []map[string]any{
		{
			"text": "done",
			"file_ops": []map[string]any{
				{
					"path": "tasks/001-demo.md",
					"replace": []map[string]any{
						{"find": "- [ ]", "with": "- [x]"},
					},
				},
			},
		},
	}
	proj := setupProject(t, responses)

	// Fake HOME so fraude's JSONL writes don't pollute the user.
	t.Setenv("HOME", t.TempDir())
	// Stamp the responses path through to fraude's subprocess.
	t.Setenv("STAGENT_FAKE_RESPONSES", filepath.Join(proj.dir, "responses.json"))

	if err := os.MkdirAll(filepath.Dir(proj.dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(context.Background(), proj.dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	cfg, err := config.Load(filepath.Join(proj.dir, ".stagent.yaml"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	// Emit task.created.
	if err := store.AppendPayload(context.Background(), &events.Event{
		TaskID: 1, Type: events.EventTaskCreated, Actor: events.ActorSystem,
	}, events.TaskCreatedPayload{
		Title:       "Demo task",
		Flow:        "default",
		TaskFile:    proj.taskFilePath,
		WorktreeDir: proj.dir,
		Branch:      "task-001",
	}); err != nil {
		t.Fatal(err)
	}

	r, err := New(Options{
		WorkingDir: proj.dir,
		Store:      store,
		ConfigPath: filepath.Join(proj.dir, ".stagent.yaml"),
		Config:     cfg,
		Registry:   hooks.NewDefault(),
		ClaudeBin:  fraudeBin,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Run in a goroutine; stop the runner once the task is terminal.
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(8 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		task, err := store.Task(context.Background(), 1)
		if err == nil && task.Status == events.TaskStatusCompleted {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.Stop()
	<-done

	if !completed {
		evs, _ := store.ReadByTask(context.Background(), 1)
		var lines []string
		for _, e := range evs {
			lines = append(lines, string(e.Type)+":"+e.Stage)
		}
		t.Fatalf("task did not complete in time; events:\n  %s", strings.Join(lines, "\n  "))
	}

	// Confirm the agent actually ran and exit hooks fired.
	evs, _ := store.ReadByTask(context.Background(), 1)
	var seenSessionStart, seenSessionEnd, seenStageCompleted bool
	for _, e := range evs {
		switch e.Type {
		case events.EventSessionStarted:
			seenSessionStart = true
		case events.EventSessionEnded:
			seenSessionEnd = true
		case events.EventStageCompleted:
			if e.Stage == "code" {
				seenStageCompleted = true
			}
		}
	}
	if !seenSessionStart || !seenSessionEnd || !seenStageCompleted {
		t.Errorf("missing expected events: session.started=%v session.ended=%v stage.completed(code)=%v",
			seenSessionStart, seenSessionEnd, seenStageCompleted)
	}
}

func TestRunnerRetriesOnHookFailure(t *testing.T) {
	fraudeBin := buildFraude(t)

	// First response: edit nothing. Section_check fails → retry.
	// Second response: tick the boxes. Section_check passes.
	responses := []map[string]any{
		{"text": "I forgot to edit anything"},
		{
			"text": "now done",
			"file_ops": []map[string]any{
				{
					"path": "tasks/001-demo.md",
					"replace": []map[string]any{
						{"find": "- [ ]", "with": "- [x]"},
					},
				},
			},
		},
	}
	proj := setupProject(t, responses)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("STAGENT_FAKE_RESPONSES", filepath.Join(proj.dir, "responses.json"))

	store, err := events.Open(context.Background(), proj.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg, _ := config.Load(filepath.Join(proj.dir, ".stagent.yaml"))

	must(t, store.AppendPayload(context.Background(), &events.Event{
		TaskID: 1, Type: events.EventTaskCreated, Actor: events.ActorSystem,
	}, events.TaskCreatedPayload{
		Title:       "Demo task",
		Flow:        "default",
		TaskFile:    proj.taskFilePath,
		WorktreeDir: proj.dir,
		Branch:      "task-001",
	}))

	r, err := New(Options{
		WorkingDir: proj.dir,
		Store:      store,
		ConfigPath: filepath.Join(proj.dir, ".stagent.yaml"),
		Config:     cfg,
		Registry:   hooks.NewDefault(),
		ClaudeBin:  fraudeBin,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(12 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		task, err := store.Task(context.Background(), 1)
		if err == nil && task.Status == events.TaskStatusCompleted {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.Stop()
	<-done

	if !completed {
		t.Fatal("task did not complete after retry")
	}

	// Should have entered the `code` stage twice (initial + retry).
	attempts, err := store.StageAttempts(context.Background(), 1, "code")
	if err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Errorf("expected ≥ 2 attempts on code, got %d", attempts)
	}
}
