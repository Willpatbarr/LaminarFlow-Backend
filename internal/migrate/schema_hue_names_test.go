package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/hue"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
)

// 0051 (LAM-172) turns stored colours into hue names. Its SQL repeats hue.Of, so this
// runs the real Up section over a sweep of hexes and holds every answer to the Go one,
// then checks the documented fallbacks and that a second run changes nothing.
func TestHueNamesMigration(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	raw, err := migrations.FS.ReadFile("0051_hue_names.sql")
	if err != nil {
		t.Fatalf("read 0051: %v", err)
	}
	up := strings.SplitN(strings.SplitN(string(raw), "-- +migrate Down", 2)[0], "-- +migrate Up", 2)[1]
	down := strings.SplitN(string(raw), "-- +migrate Down", 2)[1]

	team := newTeam(t, pool, newWorkspace(t, pool, "hue-ws"), "Team hue")

	// Hexes: every Tailwind and vault value, greys, the extremes, and a stride through
	// the whole RGB cube that lands on both sides of every wheel boundary.
	hexes := []string{"#94a3b8", "#3b82f6", "#14b8a6", "#22c55e", "#f59e0b", "#c2410c", "#ef4444", "#8b5cf6",
		"#6B757D", "#4A7FB5", "#3F9EA8", "#4E9A6A", "#A8873F", "#B0724A", "#B05A55", "#8A6FB0",
		"#000000", "#ffffff", "#808080", "#ff0000", "#00ff00", "#0000ff", "#fff", "#f00", "3b82f6", "#EB5757", "#F2994A"}
	for v := 0; v < 1<<24; v += 40503 {
		hexes = append(hexes, fmt.Sprintf("#%06x", v))
	}

	type row struct{ stored, want string }
	var labels, statuses []row
	add := func(stored, want string) {
		n := len(labels)
		if _, err := pool.Exec(ctx, `INSERT INTO label (team_id, name, color) VALUES ($1::uuid, $2, $3)`,
			team, fmt.Sprintf("l%d", n), stored); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO status (team_id, name, color, position, category) VALUES ($1::uuid, $2, $3, 0, 'not_started')`,
			team, fmt.Sprintf("s%d", n), stored); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, row{fmt.Sprintf("l%d", n), want})
		statuses = append(statuses, row{fmt.Sprintf("s%d", n), want})
	}
	for _, h := range hexes {
		want, ok := hue.Of(h)
		if !ok {
			t.Fatalf("hue.Of(%q) refused a hex", h)
		}
		add(h, want)
	}
	// Fallbacks and names: not hexes, so slate; names stay, whatever their case.
	for stored, want := range map[string]string{
		"red": "slate", "orange": "slate", "": "slate", "#12345": "slate", "##aaaaaa": "slate", "rgb(1,2,3)": "slate",
		"blue": "blue", "Teal": "teal", "  VIOLET ": "violet", "rust": "rust",
	} {
		add(stored, want)
	}

	for pass := 1; pass <= 2; pass++ {
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		for table, rows := range map[string][]row{"label": labels, "status": statuses} {
			for _, r := range rows {
				var got string
				if err := pool.QueryRow(ctx, `SELECT color FROM `+table+` WHERE team_id = $1::uuid AND name = $2`, team, r.stored).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != r.want {
					t.Errorf("pass %d: %s %s migrated to %q, want %q (SQL and hue.Of disagree)", pass, table, r.stored, got, r.want)
				}
			}
		}
	}

	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'lf_hue_for'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("helper function left behind: %d, %v", left, err)
	}

	// Down gives each name its old Tailwind hex, and Up takes it back to the same name.
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	var blueHex string
	if err := pool.QueryRow(ctx, `SELECT color FROM status WHERE team_id = $1::uuid AND name = 's0'`, team).Scan(&blueHex); err != nil || !strings.HasPrefix(blueHex, "#") {
		t.Errorf("after down: %q, %v, want a hex", blueHex, err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	var back string
	_ = pool.QueryRow(ctx, `SELECT color FROM status WHERE team_id = $1::uuid AND name = 's0'`, team).Scan(&back)
	if back != statuses[0].want {
		t.Errorf("up after down: %q, want %q", back, statuses[0].want)
	}
}
