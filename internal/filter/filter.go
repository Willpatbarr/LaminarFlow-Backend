/*
╔═ filter.go ═══════════════════════════════════════════════════════════════════════════
║  filter · the language, and what a resource may say in it
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Group          struct
║      Condition      struct
║      Field          struct
║      Schema         map
║      Kind           string
║      Operator       string
║      Conjunction    string
║      Fault          struct
║      Error          struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket  →  Fields, the ticket vocabulary
║      internal/api     →  faults rendered as huma.ErrorDetail
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package filter is the filter language: a condition is a field, an operator and a
// value, conditions combine into groups, and groups nest.
//
// It knows no table names and holds no pool. A resource declares a Schema naming the
// fields it accepts and the SQL each one stands for, and this package turns a caller's
// group into a WHERE fragment plus an ordered argument list. That split is deliberate
// twice over: internal/document and internal/ticket guard their tables by failing the
// build on SQL string literals elsewhere, and a compiler that could emit those names
// would trip those guards; and a vocabulary is exactly the part that has to differ per
// resource, so it belongs where the resource does.
//
// The shape here is persisted, not request-only. saved_view.config is jsonb holding a
// view's filters, so changing a field name or the group encoding later is a data
// migration rather than an API version bump. Adding a field, an operator or a kind is
// free; renaming one is not.
package filter

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxDepth bounds nesting. An untrusted body may nest groups arbitrarily, and each
// level is another parenthesised subtree Postgres has to plan; five is deeper than any
// filter a person builds in a UI and shallow enough that the planner never notices.
//
// The root group is depth one, so MaxDepth 5 permits four levels of nesting under it.
const MaxDepth = 5

// MaxConditions bounds the whole tree, not one group. Depth alone does not bound size
// - a single group holding ten thousand conditions is flat and still ruinous, and the
// deployment target is a Raspberry Pi.
const MaxConditions = 100

// Conjunction joins the members of a group.
type Conjunction string

const (
	And Conjunction = "and"
	Or  Conjunction = "or"
)

// Operator is what a condition asserts about a field.
//
// is_null and is_not_null are not in API Design notes §2, and are here because
// status_id and assignee_account_id are nullable. Without them "unassigned" is
// unaskable: `= NULL` is never true in SQL, so a caller reaching for it gets an empty
// page and no error - the silent wrong answer the whole whitelist exists to prevent.
type Operator string

const (
	Eq        Operator = "eq"
	Neq       Operator = "neq"
	Gt        Operator = "gt"
	Gte       Operator = "gte"
	Lt        Operator = "lt"
	Lte       Operator = "lte"
	Contains  Operator = "contains"
	In        Operator = "in"
	IsNull    Operator = "is_null"
	IsNotNull Operator = "is_not_null"
)

// Kind is a field's type, and it is what decides which operators are legal. A string
// rather than an iota so a Field that forgets to set it is an empty Kind - which
// Schema.check rejects loudly - rather than silently the first constant.
type Kind string

const (
	Text   Kind = "text"
	UUID   Kind = "uuid"
	Time   Kind = "time"
	Number Kind = "number"
	Bool   Kind = "bool"

	// UUIDSet is a field that is not a column: label and sprint are join tables, and
	// a ticket has many of each. Its Expr is an array-valued expression rather than a
	// scalar, which is what keeps an `or` over labels and an `and` over labels the
	// same query - see compile.go.
	UUIDSet Kind = "uuid_set"
)

// legal is the operator set per kind, and it is the second half of the whitelist. A
// field name alone is not enough: `created_at contains "x"` names a real field and is
// still nonsense, and letting it through means Postgres raises the type error at
// execution time as a 500 rather than this package raising it as a 422.
var legal = map[Kind]map[Operator]bool{
	Text:    {Eq: true, Neq: true, Contains: true, In: true, IsNull: true, IsNotNull: true},
	UUID:    {Eq: true, Neq: true, In: true, IsNull: true, IsNotNull: true},
	Time:    {Eq: true, Neq: true, Gt: true, Gte: true, Lt: true, Lte: true, IsNull: true, IsNotNull: true},
	Number:  {Eq: true, Neq: true, Gt: true, Gte: true, Lt: true, Lte: true, In: true, IsNull: true, IsNotNull: true},
	Bool:    {Eq: true, Neq: true, IsNull: true, IsNotNull: true},
	UUIDSet: {Eq: true, Neq: true, In: true, IsNull: true, IsNotNull: true},
}

/*
┏━ Field ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one name a caller may filter on
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Expr    string    SQL, never caller text
┃      Kind    Kind      decides the legal operators
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      each resource's Schema literal
*/

// Field maps a filter-field name onto SQL.
//
// Expr is written by this repository and never contains caller input. That is the one
// property the whole package rests on: a caller chooses which Expr by name, and every
// value they supply arrives as a placeholder. An open mapping - a name used as a
// column - would be injection with extra steps.
type Field struct {
	Expr string
	Kind Kind
}

// Schema is a resource's whole vocabulary, keyed by the name a caller writes.
type Schema map[string]Field

// check fails fast on a Schema this repository got wrong, as distinct from a request a
// caller got wrong. A missing Kind or an empty Expr is a programming error that would
// otherwise surface as malformed SQL at request time, so it panics rather than
// returning a fault a caller would be shown.
func (s Schema) check() {
	for name, f := range s {
		if f.Expr == "" {
			panic(fmt.Sprintf("filter: field %q has no Expr", name))
		}
		if _, ok := legal[f.Kind]; !ok {
			panic(fmt.Sprintf("filter: field %q has unknown Kind %q", name, f.Kind))
		}
	}
}

/*
┏━ Condition ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one assertion about one field
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Field    string
┃      Op       Operator
┃      Value    any        absent for is_null
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      JSON decoding of a request or saved_view.config
*/

type Condition struct {
	Field string   `json:"field" required:"true" doc:"A name from this resource's filter vocabulary."`
	Op    Operator `json:"op" required:"true" doc:"One of eq, neq, gt, gte, lt, lte, contains, in, is_null, is_not_null."`
	Value any      `json:"value,omitempty" doc:"A scalar, or an array for in. Omitted for is_null and is_not_null."`
}

/*
┏━ Group ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  conditions and subgroups joined by one conjunction
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Op            Conjunction    and · or
┃      Conditions    []Condition
┃      Groups        []Group        recursive, bounded by MaxDepth
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      JSON decoding of a request or saved_view.config
*/

// Group is two parallel arrays rather than one array of a sum type, which is the
// choice that decided everything downstream.
//
// The alternative - one members array holding either shape - needs a discriminator and
// a custom UnmarshalJSON, and produces an OpenAPI schema with a oneOf that generated
// clients render badly. Two typed arrays need neither: encoding/json handles the
// recursion through the slice on its own, and huma emits a self-referencing $ref that
// a code generator turns into an ordinary recursive type.
//
// The cost is that `conditions` and `groups` have no relative order. Nothing needs one:
// both are joined by the same Op, and and/or are associative.
type Group struct {
	Op         Conjunction `json:"op" required:"true" doc:"and or or. Joins every condition and subgroup in this group."`
	Conditions []Condition `json:"conditions,omitempty"`
	Groups     []Group     `json:"groups,omitempty"`
}

/*
┏━ Fault ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one rejected condition, named by position
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Location    string    groups[0].conditions[2].field
┃      Message     string
┃      Value       any       what was rejected
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Schema.Compile          one per problem, never just the first
*/

// Fault mirrors huma.ErrorDetail field for field, so internal/api can hand a slice of
// these straight to the error envelope without a translation layer that could drift.
//
// Location is relative to the filter root. The caller prefixes its own path -
// "body.filter" - because only the caller knows where in its request body the filter
// sat. LAM-53 pinned that convention with TestValidationFailureNamesTheFieldByLocation.
type Fault struct {
	Location string
	Message  string
	Value    any
}

// Error carries every fault found, not the first.
//
// Returning one at a time makes a caller fix a filter by trial and error, one round
// trip per mistake. The error envelope's errors array exists precisely so it does not
// have to.
type Error struct {
	Faults []Fault
}

func (e *Error) Error() string {
	parts := make([]string, 0, len(e.Faults))
	for _, f := range e.Faults {
		if f.Location == "" {
			parts = append(parts, f.Message)
			continue
		}
		parts = append(parts, f.Location+": "+f.Message)
	}

	return "filter: " + strings.Join(parts, "; ")
}

// uuidPattern is checked here rather than left to Postgres. A malformed uuid reaching
// `$1::uuid` is a 22P02 at execution time, which surfaces as a 500 - a caller's typo
// reported as this server's fault, and with no location to point at.
var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// coerce turns one JSON value into the argument Postgres will receive, or says why it
// cannot. JSON numbers arrive as float64 and JSON has no time type, so both need a
// conversion that can fail, and failing here is what keeps it a 422.
func coerce(k Kind, v any) (any, string) {
	switch k {
	case Text:
		s, ok := v.(string)
		if !ok {
			return nil, "expected a string"
		}
		return s, ""

	case UUID, UUIDSet:
		s, ok := v.(string)
		if !ok {
			return nil, "expected a uuid string"
		}
		if !uuidPattern.MatchString(s) {
			return nil, "not a uuid"
		}
		return s, ""

	case Time:
		s, ok := v.(string)
		if !ok {
			return nil, "expected an RFC 3339 timestamp"
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, "not an RFC 3339 timestamp"
		}
		return t, ""

	case Number:
		switch n := v.(type) {
		case float64:
			return n, ""
		case int:
			return float64(n), ""
		}
		return nil, "expected a number"

	case Bool:
		b, ok := v.(bool)
		if !ok {
			return nil, "expected true or false"
		}
		return b, ""
	}

	return nil, "unfilterable field type"
}
