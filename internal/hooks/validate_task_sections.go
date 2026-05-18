package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/sections"
)

// ValidateTaskSections walks the configured hooks reachable from the
// task's flow and verifies that every section: reference resolves
// (or, for bare regex segments, that the parent resolves and the
// regex zero-match is permitted at task-creation time).
//
// See decisions.md §3 for the contract.
//
// args: none — the hook takes its inputs from Ctx (Config + TaskFile).
type ValidateTaskSections struct{}

func newValidateTaskSections(args map[string]any) (Hook, error) {
	if err := ensureKnownArgs("validate_task_sections", args); err != nil {
		return nil, err
	}
	return &ValidateTaskSections{}, nil
}

func (h *ValidateTaskSections) Name() string { return "validate_task_sections" }

func (h *ValidateTaskSections) Run(_ context.Context, hctx *Ctx) Result {
	if hctx.Config == nil {
		return Result{Verdict: Fail, Message: "validate_task_sections: ctx.Config is nil"}
	}

	doc, err := sections.Parse(hctx.TaskFile)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("validate_task_sections: parse: %v", err)}
	}

	var problems []string

	// 1. Structural rules from decisions.md §3.
	root := doc.Root()
	h1s := 0
	for _, c := range root.Children {
		if c.Level == 1 {
			h1s++
		}
	}
	if h1s != 1 {
		problems = append(problems, fmt.Sprintf("task file must have exactly one H1 (got %d)", h1s))
	}
	for _, dup := range findDuplicateSiblings(root) {
		problems = append(problems, fmt.Sprintf(
			"duplicate heading %q under %q (heading text must be unique within its parent)",
			dup.child, dup.parent))
	}

	// 2. Hook-reference walk.
	flow := hctx.Config.Flows["default"]
	if hctx.Task.Flow != "" {
		flow = hctx.Config.Flows[hctx.Task.Flow]
	}
	for _, stageName := range flow {
		stage, ok := hctx.Config.Stages[stageName]
		if !ok {
			problems = append(problems,
				fmt.Sprintf("flow references undefined stage %q", stageName))
			continue
		}
		for _, list := range [][]config.HookSpec{stage.Hooks.Enter, stage.Hooks.Exit} {
			for _, spec := range list {
				if err := validateOneHookRef(doc, spec); err != nil {
					problems = append(problems,
						fmt.Sprintf("stage %q hook %q: %v", stageName, spec.Name, err))
				}
			}
		}
	}

	if len(problems) > 0 {
		return Result{
			Verdict: Fail,
			Message: "validate_task_sections:\n  - " + strings.Join(problems, "\n  - "),
		}
	}
	return Result{Verdict: Pass}
}

// validateOneHookRef applies the table in decisions.md §3 to one
// HookSpec. Returns nil if the hook either takes no section refs or
// all its refs are well-formed.
func validateOneHookRef(doc *sections.Doc, spec config.HookSpec) error {
	switch spec.Name {
	case "section_check":
		secPath, _ := spec.Args["section"].(string)
		if secPath == "" {
			return nil // construction-time error handled elsewhere
		}
		// section_check with `expect: all_checked` requires ≥1 checkbox
		// for literal paths (regex defers checkbox check to runtime).
		mustHaveCheckboxes := false
		if expect, _ := spec.Args["expect"].(string); expect == "all_checked" {
			mustHaveCheckboxes = true
		}
		if err := verifySectionExists(doc, secPath, mustHaveCheckboxes); err != nil {
			return err
		}
		// on_fail.message_from_section: parent-only check for regex paths.
		if onFail, ok := spec.Args["on_fail"].(map[string]any); ok {
			if msgPath, _ := onFail["message_from_section"].(string); msgPath != "" {
				if err := verifySectionExists(doc, msgPath, false); err != nil {
					return fmt.Errorf("on_fail.message_from_section: %w", err)
				}
			}
		}
		return nil
	case "min_words":
		secPath, _ := spec.Args["section"].(string)
		if secPath == "" {
			return nil
		}
		return verifySectionExists(doc, secPath, false)
	default:
		// run_shell, validate_task_sections, and unknown hooks
		// (validated later by the registry) have no section refs.
		return nil
	}
}

// verifySectionExists resolves a path in validator mode. Bare regex
// zero-matches are allowed; everything else must resolve. If
// requireCheckboxes is true and the path resolves to a literal
// section, the section must contain at least one checkbox.
func verifySectionExists(doc *sections.Doc, pathSrc string, requireCheckboxes bool) error {
	path, err := sections.ParsePath(pathSrc)
	if err != nil {
		return fmt.Errorf("parse %q: %w", pathSrc, err)
	}
	sec, err := doc.Find(path, true)
	switch {
	case errors.Is(err, sections.ErrZeroMatchesValidator):
		// Bare regex matched zero — permitted at task-creation time.
		return nil
	case err != nil:
		return err
	}
	if requireCheckboxes && lastSegmentIsLiteral(path) {
		if cbs := sec.Checkboxes(); len(cbs) == 0 {
			return fmt.Errorf("section %q exists but has no checkboxes", pathSrc)
		}
	}
	return nil
}

func lastSegmentIsLiteral(p sections.Path) bool {
	if len(p) == 0 {
		return false
	}
	return p[len(p)-1].Kind == sections.SegLiteral
}

type duplicate struct{ parent, child string }

// findDuplicateSiblings returns every (parent, duplicated-child)
// pair in the doc. "Duplicate" means two sections with the same
// normalized heading text under the same parent.
func findDuplicateSiblings(s *sections.Section) []duplicate {
	var out []duplicate
	seen := map[string]int{}
	parent := s.Text
	if s.Level == 0 {
		parent = "<root>"
	}
	for _, c := range s.Children {
		seen[c.Text]++
		if seen[c.Text] == 2 {
			out = append(out, duplicate{parent: parent, child: c.Text})
		}
	}
	for _, c := range s.Children {
		out = append(out, findDuplicateSiblings(c)...)
	}
	return out
}
