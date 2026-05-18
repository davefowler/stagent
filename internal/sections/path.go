// Package sections implements the section-path grammar used by
// stagent hooks. A path is a `>`-separated list of segments that
// resolves to a heading inside a task file. See decisions.md §1.
//
// Two segment shapes:
//
//   - literal: exact heading text (whitespace inside collapsed).
//   - regex:   /pattern/ optionally followed by [N] index. Go regex.
//
// Examples:
//
//	"Implementation plan"
//	"Code > Notes"
//	"Reviews > /^Pass \d+$/[-1]"     -- last match
//	"Logs > /^attempt-/[0]"          -- first match
package sections

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SegmentKind discriminates between literal and regex segments.
type SegmentKind int

const (
	SegLiteral SegmentKind = iota
	SegRegex
)

// Segment is one piece of a parsed path.
type Segment struct {
	Kind SegmentKind

	// Literal: normalized heading text (whitespace collapsed).
	Literal string

	// Regex: compiled pattern and the raw source for diagnostics.
	Regex   *regexp.Regexp
	RawPat  string
	HasIdx  bool // /pat/[N] form
	Index   int  // negative counts from the end
}

// Path is an ordered list of segments.
type Path []Segment

// String renders a Path back to its canonical source form. Useful
// for error messages so the user sees their path echoed cleanly.
func (p Path) String() string {
	parts := make([]string, len(p))
	for i, s := range p {
		parts[i] = s.String()
	}
	return strings.Join(parts, " > ")
}

// String renders one Segment to canonical source form.
func (s Segment) String() string {
	if s.Kind == SegLiteral {
		return s.Literal
	}
	if s.HasIdx {
		return fmt.Sprintf("/%s/[%d]", s.RawPat, s.Index)
	}
	return "/" + s.RawPat + "/"
}

// ParsePath parses the path source per decisions.md §1.
//
// Errors are returned with a `path:` prefix so callers can surface
// them verbatim to the user.
func ParsePath(src string) (Path, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("path: empty")
	}

	rawSegments, err := splitSegments(src)
	if err != nil {
		return nil, err
	}

	out := make(Path, 0, len(rawSegments))
	for i, raw := range rawSegments {
		seg, err := parseSegment(raw)
		if err != nil {
			return nil, fmt.Errorf("path: segment %d (%q): %w", i+1, raw, err)
		}
		out = append(out, seg)
	}
	return out, nil
}

// splitSegments scans src and splits on `>` separators that lie
// outside of `/.../` regex delimiters. A `>` inside a regex is a
// parse error — decisions.md §1 explicitly disallows it.
func splitSegments(src string) ([]string, error) {
	var (
		segments []string
		cur      strings.Builder
		inRegex  bool
	)

	flush := func() {
		seg := strings.TrimSpace(cur.String())
		segments = append(segments, seg)
		cur.Reset()
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inRegex && c == '/':
			// End of regex body. Continue accumulating any [N] suffix.
			inRegex = false
			cur.WriteByte(c)
		case !inRegex && c == '/' && (cur.Len() == 0 || isSegmentLeadingWhitespace(cur.String())):
			// Start of regex segment. Allow leading whitespace before `/`.
			inRegex = true
			cur.WriteByte(c)
		case !inRegex && c == '>':
			flush()
		case inRegex && c == '>':
			return nil, fmt.Errorf("path: regex segment may not contain '>'")
		default:
			cur.WriteByte(c)
		}
	}
	if inRegex {
		return nil, fmt.Errorf("path: unterminated regex segment")
	}
	flush()

	for i, s := range segments {
		if s == "" {
			return nil, fmt.Errorf("path: empty segment at position %d", i+1)
		}
	}
	return segments, nil
}

func isSegmentLeadingWhitespace(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

// parseSegment parses one raw segment string (already trimmed).
func parseSegment(raw string) (Segment, error) {
	if strings.HasPrefix(raw, "/") {
		return parseRegexSegment(raw)
	}
	return Segment{Kind: SegLiteral, Literal: normalizeText(raw)}, nil
}

// parseRegexSegment parses a `/pattern/` or `/pattern/[N]` segment.
// The caller has guaranteed the leading `/`.
func parseRegexSegment(raw string) (Segment, error) {
	// Find the closing slash. There is no escape mechanism in our
	// grammar (per decisions.md: no `/.../i` flag suffix; use inline
	// `(?i)`). The first `/` after position 0 closes the body.
	closeIdx := strings.Index(raw[1:], "/")
	if closeIdx < 0 {
		return Segment{}, fmt.Errorf("regex segment missing closing '/'")
	}
	pat := raw[1 : 1+closeIdx]
	tail := raw[1+closeIdx+1:]

	if pat == "" {
		return Segment{}, fmt.Errorf("regex segment has empty pattern")
	}

	re, err := regexp.Compile(pat)
	if err != nil {
		return Segment{}, fmt.Errorf("compile %q: %w", pat, err)
	}

	seg := Segment{Kind: SegRegex, Regex: re, RawPat: pat}

	tail = strings.TrimSpace(tail)
	if tail == "" {
		return seg, nil
	}

	// Expect [N] form. Be strict — any other suffix (like /i) is a
	// parse error per decisions.md.
	if !strings.HasPrefix(tail, "[") || !strings.HasSuffix(tail, "]") {
		return Segment{}, fmt.Errorf("regex segment suffix %q not understood; expected [N] index", tail)
	}
	idxStr := tail[1 : len(tail)-1]
	n, err := strconv.Atoi(idxStr)
	if err != nil {
		return Segment{}, fmt.Errorf("regex segment index %q: %w", idxStr, err)
	}
	seg.HasIdx = true
	seg.Index = n
	return seg, nil
}

// normalizeText collapses internal whitespace runs to single spaces
// and trims edges. Decisions.md §1: "Whitespace inside the name is
// collapsed (matching 'Review  Plan' against `## Review Plan`)."
func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
