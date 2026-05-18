package sections

import "testing"

func TestCheckboxesBasic(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, "Implementation plan")
	cbs := s.Checkboxes()
	if len(cbs) != 3 {
		t.Fatalf("got %d checkboxes, want 3: %+v", len(cbs), cbs)
	}
	wantChecked := []bool{false, false, true}
	for i, want := range wantChecked {
		if cbs[i].Checked != want {
			t.Errorf("cb[%d]: Checked=%v, want %v (text=%q)",
				i, cbs[i].Checked, want, cbs[i].Text)
		}
	}
}

func TestCheckboxesCounts(t *testing.T) {
	d := mustParse(t, sampleDoc)
	s := mustFind(t, d, `Reviews > /^Pass \d+$/[-1]`)
	checked, unchecked := s.CheckboxCounts()
	if checked != 3 || unchecked != 0 {
		t.Errorf("Pass 2: got %d checked / %d unchecked, want 3/0",
			checked, unchecked)
	}

	s = mustFind(t, d, `Reviews > /^Pass \d+$/[0]`)
	checked, unchecked = s.CheckboxCounts()
	if checked != 1 || unchecked != 2 {
		t.Errorf("Pass 1: got %d/%d, want 1/2", checked, unchecked)
	}
}

func TestCheckboxesCaseInsensitive(t *testing.T) {
	src := `# t

## S

- [X] big-x
- [x] small-x
- [ ] none
`
	d := mustParse(t, src)
	s := mustFind(t, d, "S")
	cbs := s.Checkboxes()
	if len(cbs) != 3 {
		t.Fatalf("got %d, want 3", len(cbs))
	}
	if !cbs[0].Checked || !cbs[1].Checked || cbs[2].Checked {
		t.Errorf("unexpected: %+v", cbs)
	}
}

func TestCheckboxesIgnoresHTMLComments(t *testing.T) {
	src := `# t

## S

<!-- - [ ] this is commented out -->
- [ ] real one
<!--
- [ ] multi-line commented
-->
- [x] another real one
`
	d := mustParse(t, src)
	s := mustFind(t, d, "S")
	cbs := s.Checkboxes()
	if len(cbs) != 2 {
		t.Fatalf("got %d, want 2 (commented ones must be ignored): %+v", len(cbs), cbs)
	}
	if cbs[0].Checked || !cbs[1].Checked {
		t.Errorf("unexpected: %+v", cbs)
	}
}

func TestCheckboxesNotConfusedByNonCheckboxBrackets(t *testing.T) {
	src := `# t

## S

- regular item
- [a] not a checkbox
- [12] also not
- [ ] real
`
	d := mustParse(t, src)
	s := mustFind(t, d, "S")
	cbs := s.Checkboxes()
	if len(cbs) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(cbs), cbs)
	}
	if cbs[0].Checked || cbs[0].Text != "real" {
		t.Errorf("unexpected: %+v", cbs[0])
	}
}

func TestStripHTMLCommentsUnterminated(t *testing.T) {
	// Unterminated comment is left as-is. We don't want to silently
	// swallow the rest of the document.
	in := "before <!-- still open\n- [ ] item"
	got := stripHTMLComments(in)
	if got != in {
		t.Errorf("got %q, want unchanged", got)
	}
}
