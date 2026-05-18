package sections

import (
	"errors"
	"strings"
	"testing"
)

const sampleDoc = `# Fix login redirect bug

## Problem

The redirect logic in auth.go drops the original URL on a stale
session token.

## Implementation plan

- [ ] Reproduce with a minimal failing test
- [ ] Capture the URL before the redirect
- [x] Add the regression test

## Code

Filled by the developer.

### Notes

Implementation details go here.

### Other notes

Something else.

## Reviews

### Pass 1

- [ ] Tests cover the new behavior
- [x] Public API documented
- [ ] Review approved

Notes from Pass 1.

### Pass 2

- [x] Tests cover the new behavior
- [x] Public API documented
- [x] Review approved

LGTM.
`

func mustParse(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func mustFind(t *testing.T, d *Doc, src string) *Section {
	t.Helper()
	p, err := ParsePath(src)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", src, err)
	}
	s, err := d.Find(p, false)
	if err != nil {
		t.Fatalf("Find(%q): %v", src, err)
	}
	return s
}

func TestFindLiteralH2(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, "Implementation plan")
	if s.Level != 2 {
		t.Errorf("Level: got %d, want 2", s.Level)
	}
	if s.Text != "Implementation plan" {
		t.Errorf("Text: %q", s.Text)
	}
	body := s.Markdown()
	if !strings.Contains(body, "Reproduce with a minimal failing test") {
		t.Errorf("body missing expected text:\n%s", body)
	}
	// Body must NOT include the next H2's heading line.
	if strings.Contains(body, "## Code") {
		t.Errorf("body leaked into next section:\n%s", body)
	}
}

func TestFindNestedH3(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, "Code > Notes")
	if s.Level != 3 {
		t.Errorf("Level: got %d, want 3", s.Level)
	}
	if !strings.Contains(s.Markdown(), "Implementation details go here") {
		t.Errorf("body: %q", s.Markdown())
	}
}

func TestFindLiteralNotFound(t *testing.T) {
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath("Nonexistent")
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "section not found") {
		t.Errorf("error: %v", err)
	}
}

func TestFindRegexLastMatch(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, `Reviews > /^Pass \d+$/[-1]`)
	if s.Text != "Pass 2" {
		t.Errorf("Text: got %q, want Pass 2", s.Text)
	}
}

func TestFindRegexFirstMatch(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, `Reviews > /^Pass \d+$/[0]`)
	if s.Text != "Pass 1" {
		t.Errorf("Text: got %q, want Pass 1", s.Text)
	}
}

func TestFindRegexOutOfRange(t *testing.T) {
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath(`Reviews > /^Pass \d+$/[5]`)
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error: %v", err)
	}
}

func TestFindRegexNegativeOutOfRange(t *testing.T) {
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath(`Reviews > /^Pass \d+$/[-5]`)
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestFindBareRegexMultipleMatches(t *testing.T) {
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath(`Reviews > /^Pass \d+$/`)
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "specify an index") {
		t.Errorf("error: %v", err)
	}
}

func TestFindBareRegexZeroMatchesNonValidator(t *testing.T) {
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath(`Reviews > /^Nonexistent-/`)
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "matched no sections") {
		t.Errorf("error: %v", err)
	}
}

func TestFindBareRegexZeroMatchesValidator(t *testing.T) {
	// Decisions.md §1 / §3: validator allows bare-regex zero
	// matches at task-creation time (Reviews has no Pass N yet).
	src := `# title

## Reviews

(empty for now)
`
	d := mustParse(t, src)
	p, _ := ParsePath(`Reviews > /^Pass \d+$/`)
	_, err := d.Find(p, true)
	if !errors.Is(err, ErrZeroMatchesValidator) {
		t.Fatalf("want ErrZeroMatchesValidator, got %v", err)
	}
}

func TestDirectChildrenOnly(t *testing.T) {
	// `/.*/[0]` after `Code` should match the H3 "Notes" — NOT the
	// (non-existent) H4 deeper. We assert it picks Notes, not
	// "Other notes" out of order.
	d := mustParse(t, sampleDoc)
	p, _ := ParsePath(`Code > /^Notes$/`)
	s, err := d.Find(p, false)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if s.Text != "Notes" {
		t.Errorf("got %q", s.Text)
	}
}

func TestAmbiguousLiteral(t *testing.T) {
	// Two H3 "Dup" under the same H2. Real task files shouldn't have
	// this (the validator catches it), but the matcher must error
	// clearly if asked.
	src := `# title

## Outer

### Dup

a

### Dup

b
`
	d := mustParse(t, src)
	p, _ := ParsePath("Outer > Dup")
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error: %v", err)
	}
}

func TestMarkdownExtractStopsAtNextHeading(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, "Code")
	body := s.Markdown()
	if strings.Contains(body, "## Reviews") {
		t.Errorf("body should not include Reviews heading: %q", body)
	}
	if !strings.Contains(body, "### Notes") {
		t.Errorf("body should include the Notes sub-heading: %q", body)
	}
}

func TestWords(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, "Problem")
	w := s.Words()
	if len(w) < 5 {
		t.Errorf("expected several words, got %d: %v", len(w), w)
	}
}

func TestMissingH1FailsClearly(t *testing.T) {
	// Decisions.md §3 requires exactly one H1 (the validator
	// enforces this). The matcher does not silently succeed when
	// the precondition is broken — it errors and lets the validator
	// surface the structural problem.
	src := `## Foo

body
`
	d := mustParse(t, src)
	p, _ := ParsePath("Foo")
	_, err := d.Find(p, false)
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
