package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/davefowler/stagent/internal/sections"
)

// MinWords checks that a section has at least N whitespace-separated
// tokens in its body. Used to catch "agent left the section empty"
// without imposing a brittle exact-format requirement.
//
// args:
//   - section (required, string): path per decisions.md §1
//   - min (required, int, ≥ 1)
//   - file (optional, string): overrides Ctx.TaskFilePath
type MinWords struct {
	sectionPath sections.Path
	sectionSrc  string
	min         int
	file        string
}

func newMinWords(args map[string]any) (Hook, error) {
	if err := ensureKnownArgs("min_words", args, "section", "min", "file"); err != nil {
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
	min, err := argInt(args, "min", 0)
	if err != nil {
		return nil, err
	}
	if min <= 0 {
		return nil, fmt.Errorf("min must be ≥ 1, got %d", min)
	}
	file, err := argOptionalString(args, "file")
	if err != nil {
		return nil, err
	}
	return &MinWords{
		sectionPath: path,
		sectionSrc:  sectionSrc,
		min:         min,
		file:        file,
	}, nil
}

func (h *MinWords) Name() string { return "min_words" }

func (h *MinWords) Run(_ context.Context, hctx *Ctx) Result {
	src := hctx.TaskFile
	if h.file != "" {
		b, err := os.ReadFile(h.file)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Result{Verdict: Fail, Message: fmt.Sprintf("min_words: file %q does not exist", h.file)}
			}
			return Result{Verdict: Fail, Message: fmt.Sprintf("min_words: read %q: %v", h.file, err)}
		}
		src = b
	}

	doc, err := sections.Parse(src)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("min_words: parse: %v", err)}
	}
	sec, err := doc.Find(h.sectionPath, false)
	if err != nil {
		return Result{Verdict: Fail, Message: fmt.Sprintf("min_words %q: %v", h.sectionSrc, err)}
	}
	got := len(sec.Words())
	if got < h.min {
		return Result{
			Verdict: Fail,
			Message: fmt.Sprintf("min_words %q: section has %d words, need ≥ %d",
				h.sectionSrc, got, h.min),
		}
	}
	return Result{Verdict: Pass}
}
