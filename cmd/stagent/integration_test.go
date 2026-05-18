package main

// CLI-level integration test: stagent init → write task → stagent
// new --flow demo → stagent run (with fraude). Exercises the same
// path a user follows.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davefowler/stagent/internal/events"
)

func buildBinaries(t *testing.T) (stagentBin, fraudeBin string) {
	t.Helper()
	dir := t.TempDir()
	stagentBin = filepath.Join(dir, "stagent")
	fraudeBin = filepath.Join(dir, "fraude")

	build := func(target, src string) {
		cmd := exec.Command("go", "build", "-o", target, src)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("build %s: %v", src, err)
		}
	}
	build(stagentBin, ".")
	build(fraudeBin, "../fraude")
	return stagentBin, fraudeBin
}

func TestCLIFullFlow(t *testing.T) {
	stagentBin, fraudeBin := buildBinaries(t)

	project := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate fraude's ~/.claude

	// `stagent init`
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(stagentBin, append([]string{"-C", project}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("stagent %v failed: %v\noutput:\n%s", args, err, out)
		}
		return string(out)
	}

	run("init")

	// Override the config to use a slim flow with one agent stage so
	// the test doesn't depend on a real worktree-add. We replace the
	// scaffold's default config with a minimal one.
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
	must(t, os.WriteFile(filepath.Join(project, ".stagent.yaml"), []byte(cfg), 0o644))

	// Write a stage prompt the runner expects.
	must(t, os.WriteFile(
		filepath.Join(project, ".stagent", "prompts", "stages", "code.md"),
		[]byte("Tick all checkboxes in {{.Task.TaskFile}}.\n"), 0o644))

	// Pre-write a task file with checkboxes; register it via new.
	prewritten := filepath.Join(project, "spec.md")
	must(t, os.WriteFile(prewritten, []byte("# Demo\n\n## Implementation plan\n\n- [ ] step\n"), 0o644))

	run("new", prewritten)

	// Confirm task #1 landed.
	out := run("list")
	if !strings.Contains(out, "Demo") {
		t.Fatalf("list missing Demo task:\n%s", out)
	}

	// Set up scripted responses for fraude.
	respPath := filepath.Join(project, "responses.json")
	scripted := []map[string]any{
		{
			"text": "ticked",
			"file_ops": []map[string]any{
				{
					"path": filepath.Join(project, "tasks", "001-demo.md"),
					"replace": []map[string]any{
						{"find": "- [ ]", "with": "- [x]"},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(scripted, "", "  ")
	must(t, os.WriteFile(respPath, b, 0o644))

	// `stagent run` in the background.
	runCmd := exec.Command(stagentBin, "-C", project, "run", "--claude-bin", fraudeBin)
	runCmd.Env = append(os.Environ(),
		"HOME="+os.Getenv("HOME"),
		"STAGENT_FAKE_RESPONSES="+respPath)
	runCmd.Stdout = os.Stderr
	runCmd.Stderr = os.Stderr
	if err := runCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		runCmd.Process.Signal(os.Interrupt)
		runCmd.Wait()
	}()

	// Poll for task.completed.
	store, err := events.Open(context.Background(), filepath.Join(project, ".stagent", "stagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		t1, err := store.Task(context.Background(), 1)
		if err == nil && t1.Status == events.TaskStatusCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t1, err := store.Task(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Status != events.TaskStatusCompleted {
		evs, _ := store.ReadByTask(context.Background(), 1)
		var lines []string
		for _, e := range evs {
			lines = append(lines, string(e.Type)+":"+e.Stage)
		}
		t.Fatalf("task didn't complete; final status=%s; events:\n  %s",
			t1.Status, strings.Join(lines, "\n  "))
	}

	// status command works against a real runner+db.
	statusOut := run("status")
	if !strings.Contains(statusOut, "completed") {
		t.Errorf("status output missing 'completed':\n%s", statusOut)
	}
}

func TestCLIInitIdempotent(t *testing.T) {
	stagentBin, _ := buildBinaries(t)
	project := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(stagentBin, append([]string{"-C", project}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("stagent %v failed: %v\noutput:\n%s", args, err, out)
		}
		return string(out)
	}

	first := run("init")
	if !strings.Contains(first, "created: .stagent.yaml") {
		t.Errorf("first run missing 'created: .stagent.yaml':\n%s", first)
	}

	second := run("init")
	if !strings.Contains(second, "skipped: .stagent.yaml (exists)") {
		t.Errorf("second run should report skipped:\n%s", second)
	}
	if strings.Contains(second, "created: .stagent.yaml") {
		t.Errorf("second run shouldn't recreate:\n%s", second)
	}
}

func TestCLINewRejectsInvalidTask(t *testing.T) {
	stagentBin, _ := buildBinaries(t)
	project := t.TempDir()

	run := func(t *testing.T, expectFail bool, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(stagentBin, append([]string{"-C", project}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	_, err := run(t, false, "init")
	if err != nil {
		t.Fatal(err)
	}

	// Use a config that requires "Implementation plan" with checkboxes.
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
`) + "\n"
	must(t, os.WriteFile(filepath.Join(project, ".stagent.yaml"), []byte(cfg), 0o644))

	// Write a task missing the required section.
	bad := filepath.Join(project, "bad.md")
	must(t, os.WriteFile(bad, []byte("# Bad\n\n## Wrong section\n\nnothing\n"), 0o644))

	out, err := run(t, true, "new", bad)
	if err == nil {
		t.Fatalf("expected stagent new to fail; output:\n%s", out)
	}
	if !strings.Contains(out, "Implementation plan") {
		t.Errorf("error didn't mention missing section:\n%s", out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
