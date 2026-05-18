package sections

import (
	"regexp"
	"strings"
)

// Checkbox is one task-list item in a section body.
type Checkbox struct {
	Checked bool
	// Text is the trailing content of the list item, with leading
	// whitespace trimmed but markdown formatting (bold, links, etc.)
	// preserved.
	Text string
	// Line is the 1-based line number within the section body where
	// the checkbox appeared. Useful for diagnostic messages.
	Line int
}

// checkboxRE matches a markdown task-list item. We accept `-`, `*`,
// or `+` as bullets per CommonMark, with arbitrary leading
// whitespace (covers nested lists too). The character inside the
// brackets is space (unchecked) or x/X (checked); anything else is
// treated as a normal list item and not counted.
var checkboxRE = regexp.MustCompile(`^[ \t]*[-*+][ \t]+\[([ xX])\][ \t]+(.*)$`)

// Checkboxes returns every checkbox found in the section's body.
// Decisions.md §2: a section with zero checkboxes is an authoring
// error from the section_check hook's POV; the caller is responsible
// for that policy. Here we just enumerate.
//
// HTML comments are skipped per the rule in config.md: "HTML
// comments (<!-- ... -->) inside a section are ignored." The skip
// is line-granular; multi-line HTML comments are stripped first.
func (s *Section) Checkboxes() []Checkbox {
	body := s.Markdown()
	body = stripHTMLComments(body)

	var out []Checkbox
	for i, line := range strings.Split(body, "\n") {
		m := checkboxRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, Checkbox{
			Checked: m[1] == "x" || m[1] == "X",
			Text:    strings.TrimSpace(m[2]),
			Line:    i + 1,
		})
	}
	return out
}

// CheckboxCounts returns (checked, unchecked) tallies.
func (s *Section) CheckboxCounts() (checked, unchecked int) {
	for _, cb := range s.Checkboxes() {
		if cb.Checked {
			checked++
		} else {
			unchecked++
		}
	}
	return checked, unchecked
}

// stripHTMLComments removes `<!-- ... -->` spans from src. Used by
// Checkboxes so a commented-out checkbox doesn't get counted. The
// implementation is greedy and handles multi-line comments — we
// scan for `<!--` and skip until the next `-->`.
func stripHTMLComments(src string) string {
	var out strings.Builder
	i := 0
	for i < len(src) {
		j := strings.Index(src[i:], "<!--")
		if j < 0 {
			out.WriteString(src[i:])
			break
		}
		out.WriteString(src[i : i+j])
		end := strings.Index(src[i+j+4:], "-->")
		if end < 0 {
			// Unterminated comment — leave it as-is, conservative.
			out.WriteString(src[i+j:])
			break
		}
		i = i + j + 4 + end + 3
	}
	return out.String()
}
