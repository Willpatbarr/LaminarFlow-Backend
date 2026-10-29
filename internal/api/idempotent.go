/*
╔═ idempotent.go ═══════════════════════════════════════════════════════════════════════
║  Idempotency-Key for the creates that share the idempotency_key table (LAM-127)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      KeyedInput         embed in a create's input: the header, its checks, key()
║      createdOutput[B]   body plus Idempotent-Replayed
║      keyed              runs one create through the shared store
║      WithIdempotency    hands the store to NewHumaAPI
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      teams, projects, statuses, epics, sprints, milestones, labels, boards and columns,
║      saved views, aspect types, imports and fields
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"

	"github.com/danielgtaylor/huma/v2"
)

// WithIdempotency gives the creates that have no key column of their own the table that
// holds their keys. NewMux always passes one; without it (cmd/openapi) no handler runs.
func WithIdempotency(s *idempotency.Store) APIOption {
	return func(o *apiOptions) { o.idem = s }
}

// KeyedInput is the Idempotency-Key header, for embedding in a create's input struct. The
// tag is the one the ticket create carries, so the OpenAPI document describes every create
// the same way.
type KeyedInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the create safe to retry. The same key from the same account, on this operation, returns the resource the first call created instead of creating another - its current state, with Idempotent-Replayed: true, and no second change-feed event. 1 to 128 printable ASCII characters; a UUID is ideal. Keys are per account and operation. A replay ignores the body it carries: send a new key for a different resource. If the resource has since been deleted, the key is free and the request creates anew. Omit it for a plain create."`
}

func (in *KeyedInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

// key is the header as the store takes it: nil when none was sent.
func (in *KeyedInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

// createdOutput is a create's response: the resource, and whether it is a replay. The
// status is 200 for both, as for tickets - a fresh create has always answered 200.
type createdOutput[B any] struct {
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used, nothing was created, and the body is the original resource as it is now. false: this request created it."`
	Body     B
}

func created[B any](key *string, replayed bool, body B) *createdOutput[B] {
	out := &createdOutput[B]{Body: body}
	if key != nil {
		out.Replayed = "false"
		if replayed {
			out.Replayed = "true"
		}
	}

	return out
}

// keyed runs create once per (account, key, operation). A replay returns load's answer and
// the handler must not publish: the first create already told the change feed.
func keyed[T any](ctx context.Context, store *idempotency.Store, accountID string, key *string, operation string,
	gone error,
	create func(context.Context) (T, string, error),
	load func(context.Context, string) (T, error),
) (T, bool, error) {
	v, replayed, err := idempotency.Do(ctx, store, idempotency.Request{AccountID: accountID, Key: key, Operation: operation},
		create, load, gone)
	if errors.Is(err, idempotency.ErrKeyShape) {
		return v, false, newFieldError(defaultCode(http.StatusUnprocessableEntity),
			"the Idempotency-Key was not accepted", "header."+idempotencyHeader, err.Error())
	}

	return v, replayed, err
}
