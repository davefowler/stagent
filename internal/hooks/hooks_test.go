package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
)

func mustBuild(t *testing.T, name string, args map[string]any) Hook {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	h, err := NewDefault().Build(config.HookSpec{Name: name, Args: args})
	if err != nil {
		t.Fatalf("Build %s: %v", name, err)
	}
	return h
}

func TestRegistryUnknownHook(t *testing.T) {
	_, err := NewDefault().Build(config.HookSpec{Name: "nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown hook") {
		t.Fatalf("got %v, want unknown-hook error", err)
	}
}

func TestRegistryDefaultNames(t *testing.T) {
	got := NewDefault().Names()
	want := []string{"min_words", "run_shell", "section_check", "validate_task_sections"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("default registry names: got %v, want %v", got, want)
	}
}

// ─── run_shell ───────────────────────────────────────────────────────

func TestRunShellPasses(t *testing.T) {
	h := mustBuild(t, "run_shell", map[string]any{"cmd": "true"})
	r := h.Run(context.Background(), &Ctx{WorkingDir: t.TempDir()})
	if r.Verdict != Pass {
		t.Errorf("got %s (%q), want Pass", r.Verdict, r.Message)
	}
}

func TestRunShellFailsOnNonzero(t *testing.T) {
	h := mustBuild(t, "run_shell", map[string]any{"cmd": "false"})
	r := h.Run(context.Background(), &Ctx{WorkingDir: t.TempDir()})
	if r.Verdict != Fail {
		t.Errorf("got %s, want Fail", r.Verdict)
	}
	if !strings.Contains(r.Message, "exit 1") {
		t.Errorf("message: %q", r.Message)
	}
}

func TestRunShellFailOnNonzeroFalse(t *testing.T) {
	h := mustBuild(t, "run_shell", map[string]any{
		"cmd": "exit 7", "fail_on_nonzero": false,
	})
	r := h.Run(context.Background(), &Ctx{WorkingDir: t.TempDir()})
	if r.Verdict != Pass {
		t.Errorf("got %s, want Pass (fail_on_nonzero=false)", r.Verdict)
	}
}

func TestRunShellTemplating(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "marker")
	h := mustBuild(t, "run_shell", map[string]any{
		"cmd": "echo {{.Task.Title}} > " + out,
	})
	r := h.Run(context.Background(), &Ctx{
		WorkingDir: dir,
		Task:       events.Task{Title: "hello"},
	})
	if r.Verdict != Pass {
		t.Fatalf("got %s (%q)", r.Verdict, r.Message)
	}
	b, _ := os.ReadFile(out)
	if strings.TrimSpace(string(b)) != "hello" {
		t.Errorf("marker file: %q", string(b))
	}
}

func TestRunShellTimeout(t *testing.T) {
	h := mustBuild(t, "run_shell", map[string]any{
		"cmd": "sleep 5", "timeout": "100ms",
	})
	r := h.Run(context.Background(), &Ctx{WorkingDir: t.TempDir()})
	if r.Verdict != Fail || !strings.Contains(r.Message, "timed out") {
		t.Errorf("got %s (%q), want Fail with timeout", r.Verdict, r.Message)
	}
}

func TestRunShellRejectsUnknownArg(t *testing.T) {
	_, err := NewDefault().Build(config.HookSpec{
		Name: "run_shell",
		Args: map[string]any{"cmd": "true", "typo": "value"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown arg") {
		t.Fatalf("got %v, want unknown-arg error", err)
	}
}

// ─── section_check ───────────────────────────────────────────────────

const taskDoc = `# Demo task

## Implementation plan

- [ ] First step
- [ ] Second step
- [x] Third step

## All done

- [x] One
- [x] Two

## Empty checklist

(no boxes here)
`

func TestSectionCheckAllChecked(t *testing.T) {
	h := mustBuild(t, "section_check", map[string]any{
		"section": "All done",
		"expect":  "all_checked",
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(taskDoc)})
	if r.Verdict != Pass {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestSectionCheckUncheckedFails(t *testing.T) {
	h := mustBuild(t, "section_check", map[string]any{
		"section": "Implementation plan",
		"expect":  "all_checked",
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(taskDoc)})
	if r.Verdict != Fail {
		t.Fatalf("got %s, want Fail", r.Verdict)
	}
	if !strings.Contains(r.Message, "2 of 3 checkboxes unchecked") {
		t.Errorf("message: %q", r.Message)
	}
}

func TestSectionCheckZeroCheckboxesIsFail(t *testing.T) {
	// decisions.md §2: zero checkboxes → Fail with the specific message.
	h := mustBuild(t, "section_check", map[string]any{
		"section": "Empty checklist",
		"expect":  "all_checked",
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(taskDoc)})
	if r.Verdict != Fail || !strings.Contains(r.Message, "no checkboxes") {
		t.Errorf("got %s (%q), want Fail + 'no checkboxes'", r.Verdict, r.Message)
	}
}

func TestSectionCheckOnFailRedirect(t *testing.T) {
	// Build a doc where the redirect target message_from_section lives.
	doc := `# t

## Reviews

### Pass 1

- [ ] approve
- [ ] tests cover

Reviewer says please rewrite the validation.

## Plan

- [ ] step one
`
	h := mustBuild(t, "section_check", map[string]any{
		"section": `Reviews > /^Pass \d+$/[-1]`,
		"expect":  "all_checked",
		"on_fail": map[string]any{
			"redirect_to":          "code",
			"message_from_section": `Reviews > /^Pass \d+$/[-1]`,
		},
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(doc)})
	if r.Verdict != Redirect {
		t.Fatalf("got %s, want Redirect", r.Verdict)
	}
	if r.Target != "code" {
		t.Errorf("Target: got %q, want code", r.Target)
	}
	if !strings.Contains(r.Message, "Reviewer says please rewrite") {
		t.Errorf("Message: %q", r.Message)
	}
}

func TestSectionCheckRejectsUnknownExpect(t *testing.T) {
	_, err := NewDefault().Build(config.HookSpec{
		Name: "section_check",
		Args: map[string]any{"section": "x", "expect": "some_checked"},
	})
	if err == nil || !strings.Contains(err.Error(), "only \"all_checked\"") {
		t.Fatalf("got %v, want expect error", err)
	}
}

// ─── min_words ───────────────────────────────────────────────────────

func TestMinWordsPasses(t *testing.T) {
	h := mustBuild(t, "min_words", map[string]any{
		"section": "Implementation plan", "min": 3,
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(taskDoc)})
	if r.Verdict != Pass {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestMinWordsFails(t *testing.T) {
	h := mustBuild(t, "min_words", map[string]any{
		"section": "Empty checklist", "min": 50,
	})
	r := h.Run(context.Background(), &Ctx{TaskFile: []byte(taskDoc)})
	if r.Verdict != Fail || !strings.Contains(r.Message, "need ≥ 50") {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestMinWordsRequiresPositive(t *testing.T) {
	_, err := NewDefault().Build(config.HookSpec{
		Name: "min_words",
		Args: map[string]any{"section": "x", "min": 0},
	})
	if err == nil || !strings.Contains(err.Error(), "≥ 1") {
		t.Fatalf("got %v", err)
	}
}

// ─── validate_task_sections ─────────────────────────────────────────

// validatorCfg is the smallest flow we can validate against. The
// code stage references "Implementation plan" with all_checked —
// the validator should require that section to exist and have ≥1 box.
func validatorCfg() *config.Config {
	cfg := &config.Config{
		Roles: map[string]config.Role{
			"dev": {Model: "opus", Bound: config.BoundTask},
		},
		Stages: map[string]config.Stage{
			"setup": {Type: config.StageScript},
			"code": {Type: config.StageAgent, Role: "dev", Hooks: config.StageHooks{
				Exit: []config.HookSpec{
					{Name: "section_check", Args: map[string]any{
						"section": "Implementation plan",
						"expect":  "all_checked",
					}},
				},
			}},
		},
		Flows: map[string]config.Flow{"default": {"setup", "code"}},
	}
	// apply defaults so max_runs etc. are populated.
	_, _ = config.Parse([]byte(""))
	return cfg
}

func TestValidatorPassesValidDoc(t *testing.T) {
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(taskDoc),
		Config:   validatorCfg(),
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Pass {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestValidatorCatchesMissingSection(t *testing.T) {
	missingSection := `# t

## Some other section

- [ ] foo
`
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(missingSection),
		Config:   validatorCfg(),
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Fail || !strings.Contains(r.Message, "Implementation plan") {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestValidatorCatchesSectionWithoutCheckboxes(t *testing.T) {
	emptyBoxes := `# t

## Implementation plan

(empty)
`
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(emptyBoxes),
		Config:   validatorCfg(),
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Fail || !strings.Contains(r.Message, "no checkboxes") {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestValidatorCatchesMissingH1(t *testing.T) {
	noH1 := `## Implementation plan

- [ ] step
`
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(noH1),
		Config:   validatorCfg(),
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Fail || !strings.Contains(r.Message, "exactly one H1") {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestValidatorCatchesDuplicateSiblings(t *testing.T) {
	dups := `# t

## Plan

- [ ] one

## Plan

- [ ] two
`
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(dups),
		Config:   validatorCfg(),
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Fail || !strings.Contains(r.Message, "duplicate heading") {
		t.Errorf("got %s (%q)", r.Verdict, r.Message)
	}
}

func TestValidatorPermitsBareRegexZeroMatches(t *testing.T) {
	// A common pattern: review stage references "Reviews > /^Pass \d+$/" and
	// the task file starts with an empty Reviews section.
	cfg := &config.Config{
		Roles: map[string]config.Role{"dev": {Model: "opus", Bound: config.BoundTask}},
		Stages: map[string]config.Stage{
			"setup": {Type: config.StageScript},
			"review": {Type: config.StageAgent, Role: "dev", Hooks: config.StageHooks{
				Exit: []config.HookSpec{
					{Name: "section_check", Args: map[string]any{
						"section": `Reviews > /^Pass \d+$/`,
						"expect":  "all_checked",
					}},
				},
			}},
		},
		Flows: map[string]config.Flow{"default": {"setup", "review"}},
	}
	taskWithEmptyReviews := `# t

## Implementation plan

- [ ] step

## Reviews

(no passes yet)
`
	h := mustBuild(t, "validate_task_sections", nil)
	r := h.Run(context.Background(), &Ctx{
		TaskFile: []byte(taskWithEmptyReviews),
		Config:   cfg,
		Task:     events.Task{Flow: "default"},
	})
	if r.Verdict != Pass {
		t.Errorf("got %s (%q); zero matches on bare regex must be OK at validation time", r.Verdict, r.Message)
	}
}
