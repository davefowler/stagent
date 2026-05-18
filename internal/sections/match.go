package sections

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// ErrNotFound is returned when a path fails to resolve. Callers
// check it with errors.Is — the underlying error message includes
// the specific reason ("section not found", "ambiguous section
// path", "regex matched no sections", etc.) per decisions.md §1.
var ErrNotFound = errors.New("section path resolution failed")

// Doc is a parsed task-file ready for path queries.
type Doc struct {
	src  []byte
	root *Section // synthetic level-0 wrapper; its Children are the H1s
}

// Section represents one heading and the source range it covers.
// Body is [HeadingEnd+1, BodyEnd) where HeadingEnd is the newline at
// the end of the heading line; the byte at HeadingEnd is '\n' (or
// len(src) at EOF without trailing newline).
type Section struct {
	doc *Doc

	Level    int      // 0 for the synthetic root, 1 for H1, etc.
	Text     string   // normalized heading text (collapsed whitespace)
	Heading  ast.Node // nil for the synthetic root

	HeadingStart int // byte offset of the line containing the heading
	HeadingEnd   int // byte offset of the trailing newline (or len(src))
	BodyEnd      int // byte offset where the section's body ends

	parent   *Section
	Children []*Section
}

// Parse parses the task file source into a navigable section tree.
// goldmark is used for heading detection only; everything else uses
// raw source positions so we can hand callers the exact markdown.
func Parse(src []byte) (*Doc, error) {
	md := goldmark.New()
	reader := text.NewReader(src)
	rootNode := md.Parser().Parse(reader)

	doc := &Doc{src: src}
	doc.root = &Section{doc: doc, Level: 0, BodyEnd: len(src)}

	// Walk the document collecting top-level headings in source order.
	// We don't recurse into block children — headings only appear at
	// the document top level in CommonMark.
	type collected struct {
		node      *ast.Heading
		text      string
		lineStart int
		lineEnd   int
	}
	var found []collected

	for n := rootNode.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok {
			continue
		}
		ls, le, err := headingLineBounds(src, h)
		if err != nil {
			return nil, err
		}
		found = append(found, collected{
			node:      h,
			text:      normalizeText(string(h.Text(src))),
			lineStart: ls,
			lineEnd:   le,
		})
	}

	// Build the tree with a stack keyed by Level.
	stack := []*Section{doc.root}
	for _, c := range found {
		// Pop until the top has Level < c.node.Level.
		for len(stack) > 1 && stack[len(stack)-1].Level >= c.node.Level {
			stack[len(stack)-1].BodyEnd = c.lineStart
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		s := &Section{
			doc:          doc,
			Level:        c.node.Level,
			Text:         c.text,
			Heading:      c.node,
			HeadingStart: c.lineStart,
			HeadingEnd:   c.lineEnd,
			BodyEnd:      len(src), // updated when we pop, or stays at EOF
			parent:       parent,
		}
		parent.Children = append(parent.Children, s)
		stack = append(stack, s)
	}

	return doc, nil
}

// Root returns the synthetic level-0 section. Its Children are the
// document's H1s. Used by the validator to inspect document structure.
func (d *Doc) Root() *Section { return d.root }

// Source returns the underlying source bytes. Section.Markdown()
// slices from this.
func (d *Doc) Source() []byte { return d.src }

// Find resolves a path against the document. If the document has
// exactly one H1, resolution starts there (so a top-level path like
// "Code" matches an H2 directly under the H1). Otherwise resolution
// starts at the synthetic root, which means malformed documents fail
// with a clear error instead of silently matching the wrong thing.
//
// validatorMode = true permits a bare regex segment (no [N]) to
// match zero candidates — decisions.md §1: at task-creation time
// "## Reviews > /^Pass \d+$/" matches nothing by design.
func (d *Doc) Find(p Path, validatorMode bool) (*Section, error) {
	if len(p) == 0 {
		return nil, fmt.Errorf("%w: empty path", ErrNotFound)
	}

	start := d.startPoint()

	current := start
	for i, seg := range p {
		next, err := stepOne(current, seg, validatorMode && i == len(p)-1)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", p.String(), err)
		}
		current = next
	}
	return current, nil
}

// startPoint returns the section the first path segment matches
// children of: the lone H1 if there's exactly one, else the root
// itself.
func (d *Doc) startPoint() *Section {
	if len(d.root.Children) == 1 && d.root.Children[0].Level == 1 {
		return d.root.Children[0]
	}
	return d.root
}

// stepOne resolves one segment against the candidate children of
// `current`. Per decisions.md §1, only direct children at exactly
// (current.Level + 1) are eligible.
func stepOne(current *Section, seg Segment, validatorMode bool) (*Section, error) {
	wantLevel := current.Level + 1
	var candidates []*Section
	for _, c := range current.Children {
		if c.Level == wantLevel {
			candidates = append(candidates, c)
		}
	}

	switch seg.Kind {
	case SegLiteral:
		return resolveLiteral(seg, candidates)
	case SegRegex:
		return resolveRegex(seg, candidates, validatorMode)
	default:
		return nil, fmt.Errorf("unknown segment kind %v", seg.Kind)
	}
}

func resolveLiteral(seg Segment, candidates []*Section) (*Section, error) {
	var matches []*Section
	for _, c := range candidates {
		if c.Text == seg.Literal {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: section not found: %q", ErrNotFound, seg.Literal)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: ambiguous section path: %q matches %d sections",
			ErrNotFound, seg.Literal, len(matches))
	}
}

func resolveRegex(seg Segment, candidates []*Section, validatorMode bool) (*Section, error) {
	var matches []*Section
	for _, c := range candidates {
		if seg.Regex.MatchString(c.Text) {
			matches = append(matches, c)
		}
	}

	if seg.HasIdx {
		idx := seg.Index
		if idx < 0 {
			idx = len(matches) + idx // -1 → last, -2 → second-to-last
		}
		if idx < 0 || idx >= len(matches) {
			return nil, fmt.Errorf("%w: regex match index %d out of range; got %d matches",
				ErrNotFound, seg.Index, len(matches))
		}
		return matches[idx], nil
	}

	switch len(matches) {
	case 0:
		if validatorMode {
			return nil, ErrZeroMatchesValidator
		}
		return nil, fmt.Errorf("%w: regex matched no sections: /%s/", ErrNotFound, seg.RawPat)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: regex matched %d sections; specify an index: /%s/",
			ErrNotFound, len(matches), seg.RawPat)
	}
}

// ErrZeroMatchesValidator is a sentinel the validator hook special-
// cases. It is NOT wrapped with ErrNotFound because callers in
// validator mode treat zero-matches as "OK, regex is structurally
// valid; no body to check yet."
var ErrZeroMatchesValidator = errors.New("zero matches (validator-mode pass)")

// Markdown returns the raw markdown source between the section's
// heading line (exclusive) and the next sibling-or-ancestor heading
// (exclusive). For the synthetic root, returns the entire source.
func (s *Section) Markdown() string {
	if s.Heading == nil {
		return string(s.doc.src)
	}
	bodyStart := s.HeadingEnd
	if bodyStart < len(s.doc.src) && s.doc.src[bodyStart] == '\n' {
		bodyStart++
	}
	if bodyStart > s.BodyEnd {
		return ""
	}
	return string(s.doc.src[bodyStart:s.BodyEnd])
}

// Words returns the section body split on whitespace. Used by
// min_words. Operates on the markdown source — bold markers,
// HTML comments, etc. become part of "word" tokens. Good enough for
// "did the agent actually write anything substantive" checks.
func (s *Section) Words() []string {
	return strings.Fields(s.Markdown())
}

// headingLineBounds returns [start, end) byte offsets of the source
// line containing the heading. For ATX headings (`## Foo`) this is
// the single line including the `##` marker. For setext headings
// (rare in task files, but supported by CommonMark) the bounds
// cover only the text line; the underline `===` lands in the body.
// That's a known limitation; it doesn't affect path resolution.
func headingLineBounds(src []byte, h *ast.Heading) (int, int, error) {
	lines := h.Lines()
	if lines.Len() == 0 {
		return 0, 0, fmt.Errorf("heading without source lines")
	}
	first := lines.At(0)

	// Scan back from first.Start to find line beginning.
	start := first.Start
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	// Scan forward to find the trailing newline (or EOF).
	end := first.Stop
	for end < len(src) && src[end] != '\n' {
		end++
	}
	return start, end, nil
}
