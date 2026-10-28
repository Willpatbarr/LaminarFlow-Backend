/*
╔═ patch.go ════════════════════════════════════════════════════════════════════════════
║  http handlers · members an update may leave alone, set, or clear
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      patchValue    struct
║      patchUUID     struct
║      patchDate     struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      update-ticket    →  epic_id · milestone_id
║      update-sprint    →  start_date · end_date
║      update-milestone →  target_date
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"bytes"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
)

// patchValue tells three requests apart that a *string cannot: the member absent (leave
// it alone), null (clear it) and a value (set it). encoding/json calls UnmarshalJSON only
// for a member that is present, so Set is true exactly then, and Value is nil for null.
//
// Used where an update must not clear a column its caller has never heard of. The older
// PUT members (status_id, assignee_id) are full-replace and required; a client written
// before epic_id existed would wipe every epic it touched if the new members worked that
// way too.
type patchValue struct {
	Set   bool
	Value *string
}

func (p *patchValue) UnmarshalJSON(b []byte) error {
	p.Set = true

	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		p.Value = nil
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p.Value = &s

	return nil
}

// patchUUID is a patchValue the document describes as a nullable uuid.
type patchUUID struct{ patchValue }

func (patchUUID) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Format: "uuid", Nullable: true}
}

// patchDate is a patchValue the document describes as a nullable date (YYYY-MM-DD).
type patchDate struct{ patchValue }

func (patchDate) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Format: "date", Nullable: true}
}
