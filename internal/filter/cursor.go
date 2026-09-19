/*
╔═ cursor.go ═══════════════════════════════════════════════════════════════════════════
║  filter · a position in a sorted result, and the predicate that resumes from it
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Cursor                struct
║      ErrBadCursor          error
║      ErrCursorMismatch     error
║      Fingerprint           func
║      Schema.SortKeys       method
║      Schema.CompileAfter   method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket  →  List, one page at a time
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package filter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

// ErrBadCursor means the token did not decode. A client never builds one, so this is a
// truncated or hand-edited string rather than a state the API can be in.
var ErrBadCursor = errors.New("filter: cursor is not readable")

// DefaultLimit and MaxLimit are LAM-59 decision 3, and they live here rather than in a
// resource because the API schema and every service have to agree on them. Two copies
// would be a bound the document advertises and a bound the server enforces.
//
// Over-max is rejected, not clamped. A client that asks for 500 and silently receives
// 200 cannot tell that from a result set of exactly 200, so it stops paginating and
// shows a truncated list - the one failure mode a list endpoint must not have.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// ErrCursorMismatch means the cursor was minted under a different filter or sort.
//
// LAM-59 decision 4. The alternative - resuming anyway - returns a page that is neither
// the old sequence nor the new one, with no way for a client to notice. Detecting it
// costs one hash in the token and turns a silent wrong answer into an error a client
// can act on by restarting from the first page.
var ErrCursorMismatch = errors.New("filter: cursor was minted for a different filter or sort")

/*
┏━ Cursor ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the last row of a page, as a resumable position
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Query    string     fingerprint of the filter and sort
┃      Key      []*string  one per sort key, then the tiebreak
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Encode              at the end of every page
┃      Decode              at the start of every page after the first
*/

// Cursor is keyset, not offset. LAM-59 decision 2, and §3's reason: an OFFSET makes
// Postgres produce and discard every row before the page, so the hundredth page costs a
// hundred pages of work. On the deployment target that is the difference between a list
// that works and one that does not.
//
// Key values are the sort columns of the last row *as text*, which is what makes the
// round trip exact without a type table. Postgres' text output for timestamptz, uuid,
// numeric and text is lossless, and the predicate casts each one back to the column's
// own type - so the comparison happens between two values of the same type, never
// between a string and a timestamp.
//
// A nil entry is a genuine NULL in the sort column, which is why Key is []*string
// rather than []string. Sorting by status puts unstatused tickets at the end, and the
// page boundary can land among them.
type Cursor struct {
	Query string    `json:"q"`
	Key   []*string `json:"k"`
}

// Encode renders a cursor as the opaque token a client echoes back.
//
// Opaque so the server can change what it holds without a version negotiation. Base64
// of JSON rather than anything cleverer, because the only requirement is that a client
// treats it as bytes - and the moment it is readable, something will parse it.
func (c Cursor) Encode() string {
	body, err := json.Marshal(c)
	if err != nil {
		// Cursor holds strings and pointers to strings, which always marshal.
		panic("filter: cursor did not marshal: " + err.Error())
	}

	return base64.RawURLEncoding.EncodeToString(body)
}

/*
┌─ filter ────────────────────────────────────────
│  decodes a client's cursor token
├─ in ────────────────────────────────────────────
│      token    string     from a previous page
├─ out ───────────────────────────────────────────
│      Cursor
│      error     ErrBadCursor
*/

func Decode(token string) (Cursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}

	var c Cursor
	if err := json.Unmarshal(body, &c); err != nil {
		return Cursor{}, ErrBadCursor
	}
	if c.Query == "" || len(c.Key) == 0 {
		return Cursor{}, ErrBadCursor
	}

	return c, nil
}

/*
┌─ filter ────────────────────────────────────────
│  fingerprints the query a cursor belongs to
├─ in ────────────────────────────────────────────
│      g         Group
│      orders    []Order    already defaulted
├─ out ───────────────────────────────────────────
│      string    16 hex characters
├─ example ───────────────────────────────────────
│      same filter, same sort  →  same string
*/

// Fingerprint identifies a filter and sort together, so a cursor can refuse to resume
// under a different one.
//
// Not a security boundary and deliberately short: a client cannot forge a cursor into
// rows it may not see, because scope is applied underneath the filter and is not part
// of what this hashes. Sixteen hex characters is far past the point where an accidental
// collision between two filters a person typed matters.
//
// It hashes the effective sort - after the default is applied - so a request that omits
// sort and one that spells out the default share a fingerprint, as they should.
func Fingerprint(g Group, orders []Order) string {
	body, err := json.Marshal(struct {
		Filter Group   `json:"f"`
		Sort   []Order `json:"s"`
	}{g, orders})
	if err != nil {
		panic("filter: fingerprint did not marshal: " + err.Error())
	}

	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:8])
}

/*
┌─ filter ────────────────────────────────────────
│  the SELECT columns a cursor is built from
├─ in ────────────────────────────────────────────
│      orders     []Order
│      tiebreak   string
├─ out ───────────────────────────────────────────
│      []string   one expression per key, all ::text
│      error      *Error
├─ example ───────────────────────────────────────
│      [updated_at]  →  ["(t.updated_at)::text", "(t.id)::text"]
*/

// SortKeys are the extra columns a list query selects so it can mint the next cursor.
//
// Cast to text here rather than scanned as their own types. One scan path for every
// kind, no driver-specific decoding of uuid or numeric, and the value goes straight
// into the token - CompileAfter casts it back on the way in.
func (s Schema) SortKeys(orders []Order, tiebreak string) ([]string, error) {
	fields, err := s.sortFields(orders)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		keys = append(keys, "("+f.Expr+")::text")
	}

	return append(keys, "("+tiebreak+")::text"), nil
}

// cursorCast is the type a cursor value is cast back to. Every value arrives as text,
// so Text needs a cast here where the filter compiler needed none - a bare $1 beside a
// text column is inferred, but beside a timestamptz it is a driver-level type error
// rather than a coercion.
func cursorCast(k Kind) string {
	if k == Text {
		return "::text"
	}

	return casts[k]
}

// sortFields resolves sort field names, with the same whitelist and the same faults
// CompileOrder raises. Shared so the two cannot disagree about which fields are
// sortable - a cursor selecting a column the ORDER BY does not use is a page boundary
// in the wrong place.
func (s Schema) sortFields(orders []Order) ([]Field, error) {
	s.check()

	var (
		faults []Fault
		fields []Field
	)

	for i, o := range orders {
		at := "sort[" + strconv.Itoa(i) + "].field"

		f, ok := s[o.Field]
		if !ok {
			faults = append(faults, Fault{Location: at, Message: "unknown field", Value: o.Field})
			continue
		}
		if f.Kind == UUIDSet {
			faults = append(faults, Fault{Location: at, Message: "not sortable", Value: o.Field})
			continue
		}

		fields = append(fields, f)
	}

	if len(faults) > 0 {
		return nil, &Error{Faults: faults}
	}

	return fields, nil
}

/*
┌─ filter ────────────────────────────────────────
│  the predicate that resumes after a cursor
├─ in ────────────────────────────────────────────
│      orders     []Order
│      tiebreak   string
│      c          Cursor
│      firstArg   int
├─ out ───────────────────────────────────────────
│      string     a WHERE fragment
│      []any      arguments, in placeholder order
│      error      ErrBadCursor or *Error
├─ example ───────────────────────────────────────
│      one key  →  "(a > $1 OR a IS NULL OR (…AND t.id > $3))"
*/

// CompileAfter builds "strictly after this row in this order".
//
// The obvious form - a row comparison, (a, b) > ($1, $2) - is wrong here twice. It
// requires every key to sort the same direction, and Postgres has no row comparison
// that mixes ASC and DESC; and it has no useful meaning when a key is NULL. Both cases
// are ordinary: sort by status descending, and status is nullable.
//
// So the comparison is written out lexicographically instead:
//
//	after(k1) OR (k1 equals v1 AND (after(k2) OR (k2 equals v2 AND tiebreak > id)))
//
// with NULLS LAST folded into after(). Under NULLS LAST nothing follows a NULL in
// either direction, so after(k, NULL) is FALSE and the whole term collapses to the
// equality branch - which is how a page boundary inside the null rows still advances,
// on the tiebreak alone.
func (s Schema) CompileAfter(orders []Order, tiebreak string, c Cursor, firstArg int) (string, []any, error) {
	fields, err := s.sortFields(orders)
	if err != nil {
		return "", nil, err
	}

	// One value per sort key, plus the tiebreak. A different length means the cursor
	// was minted under a different sort, which the fingerprint should already have
	// caught - this is the check that does not depend on the caller having made it.
	if len(c.Key) != len(fields)+1 {
		return "", nil, ErrCursorMismatch
	}

	last := c.Key[len(c.Key)-1]
	if last == nil || !uuidPattern.MatchString(*last) {
		// The tiebreak is a primary key. A null or malformed one is a corrupt
		// token, not a legitimate position.
		return "", nil, ErrBadCursor
	}

	var (
		args  []any
		after = make([]string, len(fields))
		same  = make([]string, len(fields))
		n     = firstArg
	)

	// Forward, so placeholders are allocated in the order they appear in the folded
	// string below: after(k0), equals(k0), after(k1), equals(k1), … tiebreak.
	for i, f := range fields {
		v := c.Key[i]

		if v == nil {
			after[i] = ""
			same[i] = f.Expr + " IS NULL"
			continue
		}

		cast := cursorCast(f.Kind)

		cmp := ">"
		if orders[i].Desc {
			cmp = "<"
		}

		// "OR expr IS NULL" is NULLS LAST on the way in: a null sorts after
		// every value, so it is after this one too.
		after[i] = "(" + f.Expr + " " + cmp + " $" + strconv.Itoa(n) + cast +
			" OR " + f.Expr + " IS NULL)"
		args = append(args, *v)
		n++

		same[i] = "(" + f.Expr + " IS NOT DISTINCT FROM $" + strconv.Itoa(n) + cast + ")"
		args = append(args, *v)
		n++
	}

	// The tiebreak is ascending, unique and never null, so it needs neither arm of
	// the treatment above.
	sql := tiebreak + " > $" + strconv.Itoa(n) + "::uuid"
	args = append(args, *last)

	// Fold right to left, which is the order the nesting reads in.
	for i := len(fields) - 1; i >= 0; i-- {
		inner := "(" + same[i] + " AND " + sql + ")"
		if after[i] == "" {
			// after(k, NULL) is FALSE, so the disjunction is just this branch.
			sql = inner
			continue
		}
		sql = "(" + after[i] + " OR " + inner + ")"
	}

	return sql, args, nil
}
