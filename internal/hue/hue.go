/*
╔═ hue.go ══════════════════════════════════════════════════════════════════════════════
║  hue · the eight colours a status or label may carry
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Names         []string
║      ErrNotAHue    error
║      Normalise     func
║      Of            func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      team  →  CreateStatus, UpdateStatus
║      label →  Create, Update
║      migrations/migrations/0051_hue_names.sql repeats Of in SQL; internal/migrate keeps them equal
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package hue is the one place that knows which colours a status or label may have.
//
// LAM-172: the design system has eight hues (the vault's Component Lab/_shared.md) and
// a rule that nothing a user creates is drawn more saturated than the accent. The
// stored value is therefore a hue NAME, and the client picks the shade from its own
// tokens, light or dark. A hex is still accepted on write, because API clients and old
// frontends send one, but it is turned into a name before it is stored: nothing in the
// database is a hex any more.
package hue

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Names are the eight hues, in the order the pickers show them.
var Names = []string{"slate", "blue", "teal", "green", "amber", "clay", "rust", "violet"}

// ErrNotAHue is the Reason for a colour that is neither a hue name nor a hex.
var ErrNotAHue = errors.New("must be one of slate, blue, teal, green, amber, clay, rust, violet (a #rrggbb hex is converted to the nearest)")

// exact maps the hexes the product has used so far to their hue: the Tailwind values
// the old pickers stored, and the vault's own fills. The wheel below would land each
// on the same hue; listing them keeps that a fact rather than an arithmetic accident.
var exact = map[string]string{
	"94a3b8": "slate", "3b82f6": "blue", "14b8a6": "teal", "22c55e": "green",
	"f59e0b": "amber", "c2410c": "clay", "ef4444": "rust", "8b5cf6": "violet",
	"6b757d": "slate", "4a7fb5": "blue", "3f9ea8": "teal", "4e9a6a": "green",
	"a8873f": "amber", "b0724a": "clay", "b05a55": "rust", "8a6fb0": "violet",
}

type spoke struct {
	name  string
	angle float64
}

// wheel is where each hue sits on the colour wheel, in degrees. Rust appears twice so
// that angles just under 360 are near it. Same table as the frontend's statusLook.ts.
var wheel = []spoke{
	{"rust", 3}, {"clay", 23}, {"amber", 41}, {"green", 142},
	{"teal", 186}, {"blue", 210}, {"violet", 265}, {"rust", 363},
}

// greyBelow: under this saturation a colour reads as grey whatever its angle says.
const greyBelow = 0.25

/*
┌─ hue ───────────────────────────────────────────
│  turns what a client sent into a stored hue name
├─ in ────────────────────────────────────────────
│      s    string     a hue name (any case), or #rgb / #rrggbb (the # is optional on 6)
├─ out ───────────────────────────────────────────
│      name string     one of Names
│      ok   bool       false when s is neither
├─ example ───────────────────────────────────────
│      " Blue "    →  "blue"      "#3b82f6"  →  "blue"      "red"  →  not ok
*/

func Normalise(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, n := range Names {
		if s == n {
			return n, true
		}
	}

	return Of(s)
}

// Of maps a hex to its hue. Not ok when s is not a hex.
func Of(hex string) (string, bool) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hex)), "#")
	if len(h) == 3 && strings.HasPrefix(strings.TrimSpace(hex), "#") {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return "", false
	}
	if name, ok := exact[h]; ok {
		return name, true
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return "", false
	}

	r := float64((v>>16)&255) / 255
	g := float64((v>>8)&255) / 255
	b := float64(v&255) / 255

	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	spread := max - min
	light := (max + min) / 2
	sat := 0.0
	if spread != 0 {
		sat = spread / (1 - math.Abs(2*light-1))
	}
	if sat < greyBelow {
		return "slate", true
	}

	var angle float64
	switch max {
	case r:
		angle = math.Mod((g-b)/spread, 6)
	case g:
		angle = (b-r)/spread + 2
	default:
		angle = (r-g)/spread + 4
	}
	angle = math.Mod(angle*60+360, 360)

	nearest := wheel[0]
	for _, c := range wheel {
		if math.Abs(c.angle-angle) < math.Abs(nearest.angle-angle) {
			nearest = c
		}
	}

	return nearest.name, true
}
