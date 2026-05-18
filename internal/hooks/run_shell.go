package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"text/template"
	"time"
)

// RunShell executes a shell command. Templated against ctx so
// `{{.Task.WorktreeDir}}` and friends substitute at run time.
//
// args:
//   - cmd (required, string)
//   - fail_on_nonzero (optional, bool, default true)
//   - timeout (optional, duration string, default DefaultShellTimeout)
type RunShell struct {
	cmd           *template.Template
	cmdSrc        string // original template text, for error messages
	failOnNonzero bool
	timeout       time.Duration
}

func newRunShell(args map[string]any) (Hook, error) {
	if err := ensureKnownArgs("run_shell", args,
		"cmd", "fail_on_nonzero", "timeout"); err != nil {
		return nil, err
	}
	cmdSrc, err := argString(args, "cmd")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("cmd").Parse(cmdSrc)
	if err != nil {
		return nil, fmt.Errorf("parse cmd template: %w", err)
	}
	foz, err := argBool(args, "fail_on_nonzero", true)
	if err != nil {
		return nil, err
	}
	timeoutStr, err := argOptionalString(args, "timeout")
	if err != nil {
		return nil, err
	}
	timeout := DefaultShellTimeout
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return nil, fmt.Errorf("parse timeout %q: %w", timeoutStr, err)
		}
		timeout = d
	}
	return &RunShell{
		cmd:           tmpl,
		cmdSrc:        cmdSrc,
		failOnNonzero: foz,
		timeout:       timeout,
	}, nil
}

func (h *RunShell) Name() string { return "run_shell" }

func (h *RunShell) Run(ctx context.Context, hctx *Ctx) Result {
	var rendered bytes.Buffer
	if err := h.cmd.Execute(&rendered, hctx); err != nil {
		return Result{
			Verdict: Fail,
			Message: fmt.Sprintf("run_shell: template render: %v", err),
		}
	}

	c, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := exec.CommandContext(c, "/bin/sh", "-c", rendered.String())
	cmd.Dir = hctx.WorkingDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}

	if err == nil {
		return Result{Verdict: Pass}
	}

	// Context deadline / cancellation surfaces as a Fail with a
	// clear diagnostic so the agent's retry prompt explains what
	// happened.
	if c.Err() == context.DeadlineExceeded {
		return Result{
			Verdict: Fail,
			Message: fmt.Sprintf("run_shell: timed out after %s\ncmd: %s\nstderr: %s",
				h.timeout, rendered.String(), tail(stderr.String(), 4*1024)),
		}
	}

	if !h.failOnNonzero {
		// User explicitly opted out — pass even on failure. Useful
		// for `gh pr create --fill || true`-style best-effort hooks.
		return Result{Verdict: Pass}
	}

	return Result{
		Verdict: Fail,
		Message: fmt.Sprintf("run_shell: exit %d\ncmd: %s\nstderr: %s",
			exitCode, rendered.String(), tail(stderr.String(), 4*1024)),
	}
}

// tail returns the last n bytes of s, prefixed with "...\n" when
// truncated. Bounded so a runaway shell doesn't put a megabyte of
// noise into the event log.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
