package sections

import (
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // canonical String() per segment
	}{
		{
			name: "single literal",
			src:  "Implementation plan",
			want: []string{"Implementation plan"},
		},
		{
			name: "two literals",
			src:  "Code > Notes",
			want: []string{"Code", "Notes"},
		},
		{
			name: "internal whitespace collapsed",
			src:  "Review   Plan  >  Subsection   here",
			want: []string{"Review Plan", "Subsection here"},
		},
		{
			name: "bare regex",
			src:  "Reviews > /^Pass \\d+$/",
			want: []string{"Reviews", `/^Pass \d+$/`},
		},
		{
			name: "regex with positive index",
			src:  "Logs > /^attempt-/[0]",
			want: []string{"Logs", `/^attempt-/[0]`},
		},
		{
			name: "regex with negative index",
			src:  "Reviews > /^Pass \\d+$/[-1]",
			want: []string{"Reviews", `/^Pass \d+$/[-1]`},
		},
		{
			name: "loose whitespace around separator",
			src:  "  A    >   B  ",
			want: []string{"A", "B"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePath(tc.src)
			if err != nil {
				t.Fatalf("ParsePath(%q): %v", tc.src, err)
			}
			if len(p) != len(tc.want) {
				t.Fatalf("got %d segments, want %d (%v)", len(p), len(tc.want), p)
			}
			for i, want := range tc.want {
				if got := p[i].String(); got != want {
					t.Errorf("segment %d: got %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestParsePathErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // substring
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   ", "empty"},
		{"empty segment", "A > > B", "empty segment"},
		{"regex contains >", "/A>B/", "may not contain '>'"},
		{"unterminated regex", "/foo", "unterminated"},
		{"invalid regex", "/[/", "compile"},
		{"unknown suffix", "/foo/i", "expected [N] index"},
		{"non-integer index", "/foo/[abc]", "index"},
		{"empty regex pattern", "//", "empty pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePath(tc.src)
			if err == nil {
				t.Fatalf("ParsePath(%q): want error containing %q, got nil", tc.src, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParsePath(%q): got %v, want substring %q", tc.src, err, tc.want)
			}
		})
	}
}

func TestNormalizeText(t *testing.T) {
	cases := map[string]string{
		"foo":          "foo",
		"  foo  ":      "foo",
		"a  b  c":      "a b c",
		"a\tb":         "a b",
		"a\n b":        "a b",
		" Review Plan": "Review Plan",
	}
	for in, want := range cases {
		if got := normalizeText(in); got != want {
			t.Errorf("normalizeText(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestPathString(t *testing.T) {
	p, err := ParsePath("Reviews > /^Pass \\d+$/[-1]")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.String(), `Reviews > /^Pass \d+$/[-1]`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
