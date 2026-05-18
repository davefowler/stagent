package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

// minimalValid is the smallest config that passes Validate.
const minimalValid = `
roles:
  developer:
    model: opus
    bound: task

stages:
  setup:
    type: script
  code:
    type: agent
    role: developer

flows:
  default:
    - setup
    - code
`

func parseOrFatal(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v\nsrc:\n%s", err, src)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v\nsrc:\n%s", err, src)
	}
	return cfg
}

func TestParseMinimal(t *testing.T) {
	cfg := parseOrFatal(t, minimalValid)

	if cfg.TasksDir != DefaultTasksDir {
		t.Errorf("TasksDir: got %q, want default %q", cfg.TasksDir, DefaultTasksDir)
	}
	if cfg.Heartbeat.Interval.AsDuration() != DefaultHeartbeatInterval {
		t.Errorf("Heartbeat default: got %s, want %s",
			cfg.Heartbeat.Interval.AsDuration(), DefaultHeartbeatInterval)
	}
	if cfg.Stages["setup"].MaxRuns != 3 {
		t.Errorf("default max_runs: got %d, want 3", cfg.Stages["setup"].MaxRuns)
	}
	if cfg.Roles["developer"].Bound != BoundTask {
		t.Errorf("explicit bound preserved: got %q", cfg.Roles["developer"].Bound)
	}
}

func TestDefaultBoundApplied(t *testing.T) {
	src := `
roles:
  dev:
    model: opus
stages:
  s:
    type: script
flows:
  default: [s]
`
	cfg := parseOrFatal(t, src)
	if cfg.Roles["dev"].Bound != DefaultBound {
		t.Errorf("default bound: got %q, want %q", cfg.Roles["dev"].Bound, DefaultBound)
	}
}

func TestHeartbeatDurationParse(t *testing.T) {
	src := strings.Replace(minimalValid, "flows:", "heartbeat:\n  interval: 5s\nflows:", 1)
	cfg := parseOrFatal(t, src)
	if cfg.Heartbeat.Interval.AsDuration() != 5*time.Second {
		t.Errorf("got %s, want 5s", cfg.Heartbeat.Interval.AsDuration())
	}
}

func TestHeartbeatInvalidDuration(t *testing.T) {
	src := strings.Replace(minimalValid, "flows:", "heartbeat:\n  interval: notaduration\nflows:", 1)
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("got %v, want invalid duration error", err)
	}
}

func TestHookSpecMappingUnmarshal(t *testing.T) {
	src := `
roles:
  dev: { model: opus }
stages:
  s:
    type: script
    hooks:
      enter:
        - run_shell:
            cmd: "echo hi"
            fail_on_nonzero: true
        - validate_task_sections: {}
        - other_hook:
flows:
  default: [s]
`
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	hooks := cfg.Stages["s"].Hooks.Enter
	if len(hooks) != 3 {
		t.Fatalf("got %d hooks, want 3", len(hooks))
	}
	if hooks[0].Name != "run_shell" || hooks[0].Args["cmd"] != "echo hi" {
		t.Errorf("hook[0]: %+v", hooks[0])
	}
	if hooks[1].Name != "validate_task_sections" || len(hooks[1].Args) != 0 {
		t.Errorf("hook[1] (empty {}): %+v", hooks[1])
	}
	if hooks[2].Name != "other_hook" || hooks[2].Args == nil || len(hooks[2].Args) != 0 {
		t.Errorf("hook[2] (null args): %+v", hooks[2])
	}
}

func TestHookSpecRejectsScalarArgs(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
    hooks:
      enter:
        - bad: "not a mapping"
flows:
  default: [s]
`
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "args must be a mapping") {
		t.Fatalf("got %v, want 'args must be a mapping'", err)
	}
}

func TestValidationRejectsHumanStage(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  h:
    type: human
flows:
  default: [h]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "human stage type is deferred") {
		t.Fatalf("got %v, want human-deferred error", err)
	}
}

func TestValidationRejectsTickHooks(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
    hooks:
      tick:
        - some_hook: {}
flows:
  default: [s]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tick hooks are deferred") {
		t.Fatalf("got %v, want tick-deferred error", err)
	}
}

func TestValidationRejectsRunForeverBound(t *testing.T) {
	for _, bound := range []string{"run", "forever"} {
		src := strings.Replace(minimalValid, "bound: task", "bound: "+bound, 1)
		cfg, _ := Parse([]byte(src))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "reserved for v0.2") {
			t.Errorf("bound %q: got %v, want reserved-for-v0.2 error", bound, err)
		}
	}
}

func TestValidationAgentStageRequiresRole(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  c:
    type: agent
flows:
  default: [c]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "require role") {
		t.Fatalf("got %v, want require-role error", err)
	}
}

func TestValidationScriptStageForbidsRole(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
    role: d
flows:
  default: [s]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must not declare a role") {
		t.Fatalf("got %v, want script-no-role error", err)
	}
}

func TestValidationFlowReferencesUnknownStage(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
flows:
  default: [s, nonexistent]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("got %v, want unknown-stage error", err)
	}
}

func TestValidationFlowDefaultRequired(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
flows:
  other: [s]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `"default"`) {
		t.Fatalf("got %v, want missing-default error", err)
	}
}

func TestValidationFlowStageDuplicate(t *testing.T) {
	src := `
roles:
  d: { model: opus }
stages:
  s:
    type: script
flows:
  default: [s, s]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "appears at positions") {
		t.Fatalf("got %v, want duplicate-stage error", err)
	}
}

func TestValidationUnknownIdentifier(t *testing.T) {
	src := `
roles:
  Dev: { model: opus }
stages:
  s:
    type: script
flows:
  default: [s]
`
	cfg, _ := Parse([]byte(src))
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid identifier") {
		t.Fatalf("got %v, want identifier error", err)
	}
}

func TestParseRejectsUnknownTopLevel(t *testing.T) {
	src := minimalValid + "\nunknown_field: foo\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected error on unknown top-level field, got nil")
	}
}

func TestLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".stagent.yaml")
	if err := os.WriteFile(path, []byte(minimalValid), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Stages) != 2 {
		t.Errorf("got %d stages, want 2", len(cfg.Stages))
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/no/such/file.yaml")
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("got %v, want read error", err)
	}
}

// TestScaffoldYAMLValidates exercises the actual file `stagent init`
// will render into user projects. The scaffold is a text/template
// (it contains `{{.Project}}`), so we render it first with a fake
// project name and then run the same parse/validate path as Load.
// Any drift between scaffold and validator fails here before users
// hit it.
func TestScaffoldYAMLValidates(t *testing.T) {
	const scaffoldPath = "../../scaffold/.stagent.yaml"
	b, err := os.ReadFile(scaffoldPath)
	if err != nil {
		t.Fatalf("read scaffold: %v", err)
	}
	tmpl, err := template.New("scaffold").Parse(string(b))
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, map[string]string{"Project": "test-project"}); err != nil {
		t.Fatalf("template exec: %v", err)
	}
	cfg, err := Parse([]byte(buf.String()))
	if err != nil {
		t.Fatalf("Parse rendered scaffold: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rendered scaffold: %v", err)
	}
	// Sanity checks against decision 10 (scaffold ships slim flow).
	if got := cfg.Flows["default"]; len(got) != 3 ||
		got[0] != "setup" || got[1] != "code" || got[2] != "cleanup" {
		t.Errorf("default flow drift: %v", got)
	}
	if cfg.Project != "test-project" {
		t.Errorf("template substitution failed: project = %q", cfg.Project)
	}
}
