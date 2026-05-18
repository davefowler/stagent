package main

// Smoke tests for the fraude binary. These build fraude into a
// tempdir then invoke it the same way the runner will — via
// exec.Command — to catch flag-parsing and JSONL-emission
// regressions early.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildFraude compiles the binary once per test invocation.
func buildFraude(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fraude")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build fraude: %v", err)
	}
	return out
}

// withFakeHome redirects ~ to a temp dir so JSONLs don't pollute
// the user's real ~/.claude/projects.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeResponses(t *testing.T, dir string, v any) string {
	t.Helper()
	path := filepath.Join(dir, "responses.json")
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFraudeWritesJSONL(t *testing.T) {
	bin := buildFraude(t)
	home := withFakeHome(t)
	work := t.TempDir()

	respPath := writeResponses(t, work, []Response{
		{Text: "doing work"},
	})
	t.Setenv("STAGENT_FAKE_RESPONSES", respPath)

	cmd := exec.Command(bin, "-p", "Hello", "--session-id", "test-uuid",
		"--system-prompt", "you are a tester", "--dangerously-skip-permissions")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"STAGENT_FAKE_RESPONSES="+respPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fraude exited non-zero: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "doing work") {
		t.Errorf("stdout missing scripted text:\n%s", out)
	}

	// macOS resolves /var → /private/var inside the child process, so
	// the encoded-cwd uses the resolved path. Match fraude's behavior.
	resolved, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.ReplaceAll(resolved, "/", "-")
	jsonlPath := filepath.Join(home, ".claude", "projects", encoded, "test-uuid.jsonl")
	b, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read JSONL: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected ≥ 3 JSONL lines, got %d:\n%s", len(lines), b)
	}
	// Confirm shape: system, user, assistant.
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["type"] != "system" || first["session_id"] != "test-uuid" {
		t.Errorf("first line: %+v", first)
	}
}

func TestFraudeQueuePop(t *testing.T) {
	bin := buildFraude(t)
	home := withFakeHome(t)
	work := t.TempDir()

	respPath := writeResponses(t, work, []Response{
		{Text: "first"},
		{Text: "second"},
		{Text: "third"},
	})

	run := func(prompt, sess string) string {
		t.Helper()
		cmd := exec.Command(bin, "-p", prompt, "--session-id", sess)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"HOME="+home,
			"STAGENT_FAKE_RESPONSES="+respPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fraude failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run("a", "u1"); got != "first" {
		t.Errorf("first call: got %q, want first", got)
	}
	if got := run("b", "u2"); got != "second" {
		t.Errorf("second call: got %q, want second", got)
	}
	if got := run("c", "u3"); got != "third" {
		t.Errorf("third call: got %q, want third", got)
	}

	// Fourth call → cursor past end → error.
	cmd := exec.Command(bin, "-p", "d", "--session-id", "u4")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"STAGENT_FAKE_RESPONSES="+respPath)
	if err := cmd.Run(); err == nil {
		t.Error("fourth call should have failed (queue exhausted)")
	}
}

func TestFraudePromptPrefixMatch(t *testing.T) {
	bin := buildFraude(t)
	home := withFakeHome(t)
	work := t.TempDir()

	respPath := writeResponses(t, work, map[string]Response{
		"Implement the":  {Text: "implementing"},
		"Continue from":  {Text: "continuing"},
	})

	run := func(prompt string) string {
		t.Helper()
		cmd := exec.Command(bin, "-p", prompt, "--session-id", "u")
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"HOME="+home,
			"STAGENT_FAKE_RESPONSES="+respPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fraude failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := run("Implement the feature please"); got != "implementing" {
		t.Errorf("got %q, want implementing", got)
	}
	if got := run("Now Continue from where we left"); got != "continuing" {
		t.Errorf("got %q, want continuing", got)
	}
}

func TestFraudeFileOps(t *testing.T) {
	bin := buildFraude(t)
	home := withFakeHome(t)
	work := t.TempDir()

	// Seed a file the scripted op will edit.
	target := filepath.Join(work, "task.md")
	if err := os.WriteFile(target, []byte("- [ ] step one\n- [ ] step two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	respPath := writeResponses(t, work, []Response{
		{
			Text: "edited",
			FileOps: []FileOp{
				{Path: target, Replace: []ReplacePair{{Find: "- [ ]", With: "- [x]"}}},
			},
		},
	})

	cmd := exec.Command(bin, "-p", "do edits", "--session-id", "u")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"STAGENT_FAKE_RESPONSES="+respPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fraude failed: %v\n%s", err, out)
	}

	b, _ := os.ReadFile(target)
	got := string(b)
	if strings.Contains(got, "[ ]") {
		t.Errorf("expected all boxes ticked, got:\n%s", got)
	}
}

func TestFraudeExitCode(t *testing.T) {
	bin := buildFraude(t)
	home := withFakeHome(t)
	work := t.TempDir()

	respPath := writeResponses(t, work, []Response{
		{Text: "crashing", ExitCode: 137},
	})

	cmd := exec.Command(bin, "-p", "x", "--session-id", "u")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"STAGENT_FAKE_RESPONSES="+respPath)
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 137 {
		t.Fatalf("got %v, want exit code 137", err)
	}
}
