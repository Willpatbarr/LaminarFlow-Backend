/*
╔═ list.go ═════════════════════════════════════════════════════════════════════════════
║  http handlers · the list operation every resource shares
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      registerList          func
║      listSpec              struct
║      listPage              struct
║      CodeFilterInvalid     const
║      CodeBadCursor         const
║      CodeCursorMismatch    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      registerTickets  →  POST /api/v1/tickets/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

	"github.com/danielgtaylor/huma/v2"
)

// Three codes, because a client acts differently on each. A bad filter is a bug in the
// client and the errors array says where; a mismatched cursor is recoverable by
// restarting from page one; an unreadable cursor means the token was mangled in
// transit or storage.
const (
	CodeFilterInvalid  = "filter_invalid"
	CodeBadCursor      = "bad_cursor"
	CodeCursorMismatch = "cursor_mismatch"
)

// listRequest is the body of every list operation, and it is the same type for every
// resource - which is the answer to the tension LAM-59 flagged between one generic
// handler and an honest OpenAPI document.
//
// Nothing about the *shape* differs per resource. A condition is a field name, an
// operator and a value whatever it is filtering, so the schema is genuinely shared and
// the document is not lying by saying so. What differs is which field *names* are
// accepted, and those are strings checked against a whitelist at request time - a fact
// the operation description carries, generated from that same whitelist so it cannot
// go stale.
//
// Filter is a pointer so an absent filter and an empty one are the same thing at the
// service, rather than an empty Group whose Op is neither and nor or.
type listRequest struct {
	Filter *filter.Group  `json:"filter,omitempty" doc:"Conditions and nested groups. Omitted means every row the caller may see."`
	Sort   []filter.Order `json:"sort,omitempty" doc:"Sort keys, most significant first. A unique tiebreak is always appended."`
	Limit  int            `json:"limit,omitempty" minimum:"1" maximum:"200" doc:"Rows per page. Omitted means 50. Over the maximum is rejected, not clamped."`
	Cursor string         `json:"cursor,omitempty" doc:"next_cursor from the previous page. Opaque - do not construct or parse it."`
}

type listInput struct {
	Body listRequest
}

/*
┏━ listEnvelope ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one page, in the shape every resource returns
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Items         []T
┃      NextCursor    *string    null on the last page
┃      HasMore       bool
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      registerList             one per list request
*/

// listEnvelope is generic so api/openapi.json carries one envelope per resource that a
// code generator collapses into one client type, rather than six hand-written ones.
// huma's default schema namer flattens generics, so this emits ListEnvelopeTicketBody.
//
// No total count. A COUNT(*) over the same filter doubles the cost of every page, and
// nothing in the product needs an exact total - has_more is what a table renders.
type listEnvelope[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor" doc:"Pass back as cursor for the next page. Null when there is none."`
	HasMore    bool    `json:"has_more"`
}

type listOutput[T any] struct {
	Body listEnvelope[T]
}

// listPage is what a resource's adapter returns: the service's answer, already mapped
// to the body type the API publishes.
type listPage[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

/*
┏━ listSpec ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  everything one resource contributes to its list
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      OperationID    string
┃      Path           string          V1 + "/…/list"
┃      Summary        string
┃      Fields         filter.Schema   the vocabulary, for the docs
┃      Run            func            the service call
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      each resource's register function
*/

type listSpec[T any] struct {
	OperationID string
	Path        string
	Summary     string
	Fields      filter.Schema
	Run         func(ctx context.Context, accountID string, req listRequest) (listPage[T], error)
}

/*
┌─ api ───────────────────────────────────────────
│  registers one resource's list operation
├─ in ────────────────────────────────────────────
│      api     huma.API
│      spec    listSpec[T]
├─ example ───────────────────────────────────────
│      tickets  →  POST /api/v1/tickets/list
*/

// registerList is the whole list path, once. A sixth resource is a listSpec literal
// and its adapter, not a copy of this function - which is the point LAM-59 step 2
// makes and the reason the envelope is generic.
func registerList[T any](api huma.API, spec listSpec[T]) {
	huma.Register(api, huma.Operation{
		OperationID: spec.OperationID,
		Method:      http.MethodPost,
		Path:        spec.Path,
		Summary:     spec.Summary,
		Description: listDescription(spec.Fields),
		Metadata:    map[string]any{RequireAuth: true},
	}, func(ctx context.Context, in *listInput) (*listOutput[T], error) {
		me, _ := auth.FromContext(ctx)

		page, err := spec.Run(ctx, me.AccountID, in.Body)
		if err != nil {
			return nil, listError(err)
		}

		out := &listOutput[T]{Body: listEnvelope[T]{
			Items:   page.Items,
			HasMore: page.HasMore,
		}}
		// An empty slice rather than a nil one, so the JSON is [] and not null -
		// a client should not have to handle two spellings of "no rows".
		if out.Body.Items == nil {
			out.Body.Items = []T{}
		}
		if page.NextCursor != "" {
			out.Body.NextCursor = &page.NextCursor
		}

		return out, nil
	})
}

// listDescription writes the per-resource half of the contract into the operation,
// generated from the whitelist so it cannot describe fields the server would reject.
//
// It also states why a read is a POST. That argument was settled in API Design notes
// §2 - nested and/or does not encode in a query string, and caching a per-user ticket
// list buys nothing - but a POST that does not write looks like a mistake to every
// reader who was not in that conversation, so it is written where they will be.
func listDescription(fields filter.Schema) string {
	// Sorted, because api/openapi.json is committed and a map's iteration order
	// would make the file differ between two runs of an unchanged program.
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Reads a page. POST rather than GET because nested and/or filters " +
		"do not encode cleanly in a query string, and a per-caller ticket list is " +
		"not cacheable - so a GET would buy nothing and cost expressiveness. " +
		"Nothing is written.\n\nPagination is keyset: pass the previous page's " +
		"`next_cursor` back as `cursor`. A cursor is only valid for the filter and " +
		"sort it was minted under; changing either restarts from the first page.\n\n" +
		"Filterable and sortable fields:\n")

	for _, name := range names {
		b.WriteString("\n- `" + name + "` (" + string(fields[name].Kind) + ")")
	}

	b.WriteString("\n\nPage size defaults to " + strconv.Itoa(filter.DefaultLimit) +
		" and may not exceed " + strconv.Itoa(filter.MaxLimit) + ".")

	return b.String()
}

// listError maps what a list can fail with onto the envelope.
//
// A filter fault is a 422 carrying one errors entry per problem, each located at the
// condition that caused it - which is the position convention LAM-53 pinned, and the
// reason the compiler collects every fault rather than returning the first.
func listError(err error) error {
	var fe *filter.Error
	if errors.As(err, &fe) {
		out := NewError(http.StatusUnprocessableEntity, CodeFilterInvalid,
			"the filter, sort or page size was not accepted")

		for _, f := range fe.Faults {
			out.Errors = append(out.Errors, &huma.ErrorDetail{
				Message:  f.Message,
				Location: f.Location,
				Value:    f.Value,
			})
		}

		return out
	}

	switch {
	case errors.Is(err, filter.ErrCursorMismatch):
		// 400 rather than 422: the body is well formed and the cursor is a real
		// cursor, it just belongs to a different query.
		return NewError(http.StatusBadRequest, CodeCursorMismatch,
			"this cursor was minted for a different filter or sort - start again from the first page")

	case errors.Is(err, filter.ErrBadCursor):
		return NewError(http.StatusBadRequest, CodeBadCursor,
			"cursor is not readable - pass back a next_cursor unchanged")
	}

	return err
}
