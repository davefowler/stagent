package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/davefowler/stagent/internal/sections"
)

// SectionCheck verifies that a section in the task file (or in an
// optional override file) has all its checkboxes ticked. On failure,
// either fails normally (caller retries) or redirects to a stage
// with a message body sourced from another section.
//
// args:
//   - section (required, string): path per decisions.md §1
//   - expect (required, "all_checked"): only value supported in v0.1
//   - file (optional, string): overrides Ctx.TaskFilePath
//   - on_fail (optional, map):
//       redirect_to (required, string)
//       message_from_section (required, string)
type SectionCheck struct {
	sectionPath sections.Path
	sectionSrc  string
	file        string // empty = use Ctx.TaskFilePath
	onFail      *sectionCheckRedirect
}

type sectionCheckRedirect struct {
	redirectTo          string
	messageFromPath     sections.Path
	messageFromPathSrc  string
}

func newSectionCheck(args map[string]any) (Hook, error) {
	if err := ensureKnownArgs("section_check", args,
		"section", "expect", "file", "on_fail"); err != nil {
		return nil, err
	}

	sectionSrc, err := argString(args, "section")
	if err != nil {
		return nil, err
	}
	path, err := sections.ParsePath(sectionSrc)
	if err != nil {
		return nil, fmt.Errorf("section %q: %w", sectionSrc, err)
	}

	expect, err := argString(args, "expect")
	if err != nil {
		return nil, err
	}
	if expect != "all_checked" {
		return nil, fmt.Errorf("expect %q: only \"all_checked\" is supported in v0.1", expect)
	}

	file, err := argOptionalString(args, "file")
	if err != nil {
		return nil, err
	}

	onFailMap, err := argOptionalMap(args, "on_fail")
	if err != nil {
		return nil, err
	}

	hook := &SectionCheck{
		sectionPath: path,
		sectionSrc:  sectionSrc,
		file:        file,
	}

	if onFailMap != nil {
		if err := ensureKnownArgs("section_check.on_fail", onFailMap,
			"redirect_to", "message_from_section"); err != nil {
			return nil, err
		}
		redirectTo, err := argString(onFailMap, "redirect_to")
		if err != nil {
			return nil, fmt.Errorf("on_fail: %w", err)
		}
		msgPathSrc, err := argString(onFailMap, "message_from_section")
		if err != nil {
			return nil, fmt.Errorf("on_fail: %w", err)
		}
		msgPath, err := sections.ParsePath(msgPathSrc)
		if err != nil {
			return nil, fmt.Errorf("on_fail.message_from_section %q: %w", msgPathSrc, err)
		}
		hook.onFail = &sectionCheckRedirect{
			redirectTo:         redirectTo,
			messageFromPath:    msgPath,
			messageFromPathSrc: msgPathSrc,
		}
	}

	return hook, nil
}

func (h *SectionCheck) Name() string { return "section_check" }

func (h *SectionCheck) Run(_ context.Context, hctx *Ctx) Result {
	src, err := h.loadSource(hctx)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("section_check: %v", err)}
	}

	doc, err := sections.Parse(src)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("section_check: parse: %v", err)}
	}

	sec, err := doc.Find(h.sectionPath, false)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("section_check %q: %v", h.sectionSrc, err)}
	}

	cbs := sec.Checkboxes()
	if len(cbs) == 0 {
		// decisions.md §2: zero checkboxes is a Fail, not "vacuously satisfied".
		return Result{
			Verdict: Fail,
			Message: fmt.Sprintf("section_check %q: section %q has no checkboxes; "+
				"likely a typo or missing required content",
				h.sectionSrc, sec.Text),
		}
	}

	checked, unchecked := sec.CheckboxCounts()
	if unchecked == 0 {
		return Result{Verdict: Pass}
	}

	// Some boxes unchecked. Either redirect (if on_fail set) or fail.
	failMsg := fmt.Sprintf("section_check %q: %d of %d checkboxes unchecked",
		h.sectionSrc, unchecked, checked+unchecked)

	if h.onFail == nil {
		return Result{Verdict: Fail, Message: failMsg}
	}

	// Resolve the message-from section against the SAME document.
	msgSec, err := doc.Find(h.onFail.messageFromPath, false)
	if err != nil {
		return Result{
			Verdict: Fail,
			Message: fmt.Sprintf("%s\n(on_fail: could not resolve message_from_section %q: %v)",
				failMsg, h.onFail.messageFromPathSrc, err),
		}
	}
	return Result{
		Verdict: Redirect,
		Target:  h.onFail.redirectTo,
		Message: msgSec.Markdown(),
	}
}

func (h *SectionCheck) loadSource(hctx *Ctx) ([]byte, error) {
	if h.file == "" {
		return hctx.TaskFile, nil
	}
	b, err := os.ReadFile(h.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("file %q does not exist", h.file)
		}
		return nil, err
	}
	return b, nil
}
