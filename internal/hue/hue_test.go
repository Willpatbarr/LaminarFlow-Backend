package hue

import "testing"

func TestNormalise(t *testing.T) {
	for in, want := range map[string]string{
		"blue": "blue", " Blue ": "blue", "VIOLET": "violet",
		"#3b82f6": "blue", "#94a3b8": "slate", "#14b8a6": "teal", "#22c55e": "green",
		"#f59e0b": "amber", "#c2410c": "clay", "#ef4444": "rust", "#8b5cf6": "violet",
		"#6B757D": "slate", "#4A7FB5": "blue", "#3F9EA8": "teal", "#4E9A6A": "green",
		"#A8873F": "amber", "#B0724A": "clay", "#B05A55": "rust", "#8A6FB0": "violet",
		"3b82f6": "blue", "#fff": "slate", "#000": "slate", "#f00": "rust", "#0f0": "green",
		"#EB5757": "rust", "#F2994A": "clay", "#2AB8C9": "teal",
	} {
		got, ok := Normalise(in)
		if !ok || got != want {
			t.Errorf("Normalise(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}

	for _, in := range []string{"", "   ", "red", "orange", "#12345", "#1234567", "##aaaaaa", "#gggggg", "rgb(1,2,3)", "blue2", "fff"} {
		if got, ok := Normalise(in); ok {
			t.Errorf("Normalise(%q) = %q, want refused", in, got)
		}
	}
}

func TestEveryNameIsItsOwnNormalisation(t *testing.T) {
	if len(Names) != 8 {
		t.Fatalf("%d names, want 8", len(Names))
	}
	for _, n := range Names {
		if got, ok := Normalise(n); !ok || got != n {
			t.Errorf("Normalise(%q) = %q, %v", n, got, ok)
		}
	}
}
