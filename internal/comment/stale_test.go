package comment

import "testing"

func TestChangedRunes(t *testing.T) {
	for _, c := range []struct {
		before, after string
		want          int
	}{
		{"same", "same", 0},
		{"", "", 0},
		{"hello", "hallo", 1},
		{"hello world", "hello brave world", 6},
		{"abc", "", 3},
		{"", "abc", 3},
		{"aaaa", "aaa", 1},
		{"héllo 🎉 x", "héllo 🎊 x", 1}, // runes, not bytes
	} {
		if got := ChangedRunes(c.before, c.after); got != c.want {
			t.Errorf("ChangedRunes(%q, %q) = %d, want %d", c.before, c.after, got, c.want)
		}
	}
}

func TestIsStaleIsTheTenPercentRuleAlone(t *testing.T) {
	long := "Columns assume one status each, and a board shows them."
	if isStale(long, "Columns assume one status eac, and a board shows them.", 10) {
		t.Error("a typo made the approval stale")
	}
	if !isStale(long, "Every column is fed by a label now, and a board shows them.", 10) {
		t.Error("a rewritten sentence kept the approval")
	}
	// No small-text exception: 1 of 5 is 20%.
	if !isStale("hello", "hallo", 10) {
		t.Error("1 of 5 is over 10%")
	}
	// Exactly on the threshold is not over it.
	if isStale("0123456789", "012345678x", 10) {
		t.Error("1 of 10 is exactly 10%, not more than")
	}
	if isStale("", "", 10) {
		t.Error("an empty range changed")
	}
}

func TestSliceClampsToTheText(t *testing.T) {
	if got := slice("héllo", 1, 3); got != "él" {
		t.Errorf("slice = %q", got)
	}
	if got := slice("abc", 2, 9); got != "c" {
		t.Errorf("past the end = %q", got)
	}
	if got := slice("abc", 5, 9); got != "" {
		t.Errorf("wholly past the end = %q", got)
	}
}
