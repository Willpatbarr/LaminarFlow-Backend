/*
╔═ errors.go ════════════════════════════════════════════════════════════════════════════
║  error envelope · api contract
╠═ declares ═════════════════════════════════════════════════════════════════════════════
║      Error            the shape every failure takes
║      NewError         builds one with a code attached
║      CodeNotFound     the codes shipped so far
╠═ reached from ═════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  every huma-raised failure
║      notFound    →  unmatched /api/ paths
╚════════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/danielgtaylor/huma/v2"
)

/*
┏━ Error ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the one shape every failure in this API takes
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ErrorModel   huma.ErrorModel   RFC 9457, embedded and inlined
┃      Code         string            stable identifier, always present
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewError            handlers with a code to attach
┃      useErrorEnvelope    every error huma raises itself
*/

// Error is the response body of every failure, and LAM-53 settled three things
// about it. The reasons matter more than the fields.
//
// Embedded, not replaced. huma.ErrorModel is RFC 9457 problem details - type,
// title, status, detail, instance, errors - and it is already in the OpenAPI
// document cmd/openapi emits, so the generated frontend client carries it
// today. Defining our own would mean owning that schema forever and giving up
// a standard every HTTP tool already reads, to gain one field. Embedding also
// promotes GetStatus, Error and the ContentType filter, which is what keeps
// the response application/problem+json without any work here.
//
// Code is the one field RFC 9457 lacks. The standard nominates `type` - a URI -
// as the machine-readable discriminator, so a client switching on it matches a
// suffix. A short token compares exactly:
//
//	err.code === "ticket_archived"        not
//	err.type.endsWith("/ticket-archived")
//
// This API has two kinds of caller, per API Design notes section 5: the web
// app, and the scripts and agents holding API tokens. The second kind cannot
// read prose, and `detail` has to stay free to reword - which it is not, the
// moment anything branches on its text.
//
// Errors deliberately gains no per-entry code. huma.ErrorModel.Errors is
// []*huma.ErrorDetail, a concrete type: ErrorDetailer exists, but its one
// method returns a *ErrorDetail, so a custom error handed to Add() is
// flattened back into message/location/value. A per-entry code would mean
// shadowing Errors entirely and converting huma's own schema-validation output
// on every request, and two sources filling one array is how the two drift.
// The discriminator lives once, at the top, and Location carries the position:
//
//	{"message":  "unknown field",
//	 "location": "body.filter.groups[0].conditions[2].field",
//	 "value":    "assignee_name"}
//
// That Location convention is huma's own, and it is what makes a nested filter
// failure expressible without inventing anything - see LAM-58.
type Error struct {
	huma.ErrorModel
	Code string `json:"code" doc:"Stable machine-readable identifier for this kind of failure. Never reworded." example:"ticket_archived"`
}

// The codes shipped so far.
//
// One constant per code, never a literal at a call site. A code is a public
// API surface: once a script checks for it, renaming it breaks that script, so
// the set has to be enumerable and a typo has to be a compile error. Spelling
// one at a call site stores a value no client will ever match and raises
// nothing - the same silent failure LAM-52 records for setting.key.
const (
	CodeNotFound = "not_found"
)

// defaultNewError is huma's own constructor, captured before useErrorEnvelope
// replaces it. Delegating to it keeps huma's ErrorDetailer flattening in one
// place rather than reimplementing it here, where it would drift on upgrade.
var defaultNewError = huma.NewError

/*
┌─ api ───────────────────────────────────────────
│  builds the envelope with a code attached
├─ in ────────────────────────────────────────────
│      status    int      HTTP status
│      code      string   one of the constants above
│      detail    string   prose, for a person
├─ out ───────────────────────────────────────────
│      *Error    satisfies huma.StatusError
├─ example ───────────────────────────────────────
│      404, CodeNotFound  →  {"code":"not_found",…}
*/

// NewError builds the envelope for a handler that has a code to attach.
// Handlers return it as an error; *Error satisfies huma.StatusError through
// the embedded model.
func NewError(status int, code, detail string) *Error {
	return &Error{
		ErrorModel: huma.ErrorModel{
			Status: status,
			Title:  http.StatusText(status),
			Detail: detail,
		},
		Code: code,
	}
}

/*
┌─ api ───────────────────────────────────────────
│  a 422 that names the field it is about
├─ in ────────────────────────────────────────────
│      code        string    one of the constants above
│      detail      string    prose, for a person
│      location    string    body.status_id · header.Idempotency-Key
│      message     string    what is wrong with it
├─ out ───────────────────────────────────────────
│      *Error      status 422, errors[] holding the one entry
├─ example ───────────────────────────────────────
│      "body.status_id"  →  errors[0].location "body.status_id"
*/

// newFieldError is for a rule only the service can check - the status exists, the
// assignee is in the workspace - which still has to land where a schema violation does:
// a 422 whose errors[] points at the field, so a form can put the message beside the
// input. The code is what distinguishes it from the derived unprocessable_entity.
func newFieldError(code, detail, location, message string) *Error {
	e := NewError(http.StatusUnprocessableEntity, code, detail)
	e.Errors = []*huma.ErrorDetail{{Message: message, Location: location}}

	return e
}

// limitError turns a textlimit refusal into the same 422 huma's schema validation gives
// an over-long value, so the two paths cannot be told apart by a client: the location
// is body.<field> either way. Nil for any other error, so a handler's mapping can fall
// through.
//
// Reaching it takes a rule huma's schema cannot express - a title of nothing but spaces -
// or a caller that is not this API, since the schema's maxLength refuses an over-long
// value first.
func limitError(err error) *Error {
	var fe *textlimit.FieldError
	if !errors.As(err, &fe) {
		return nil
	}

	return newFieldError(defaultCode(http.StatusUnprocessableEntity),
		"the "+fe.Field+" was not accepted", "body."+fe.Field, fe.Message())
}

/*
┌─ api ───────────────────────────────────────────
│  points huma at Error instead of ErrorModel
├─ in ────────────────────────────────────────────
│      (none)
├─ example ───────────────────────────────────────
│      422 validation  →  carries "code" too
*/

// useErrorEnvelope replaces huma's error constructor with one that returns the
// envelope above, so a failure huma raises itself - a schema validation
// rejection, an unparseable body - comes back in the same shape as one a
// handler raises deliberately.
//
// huma.NewError is a package-level var, which is the extension point huma
// documents. It is still a global, so this runs once, from the one function
// that builds the API, and has to run before any error is constructed.
func useErrorEnvelope() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		base, ok := defaultNewError(status, msg, errs...).(*huma.ErrorModel)
		if !ok {
			// huma's own constructor returns *ErrorModel and has since v2. The
			// check is here so a future change surfaces as a missing code
			// rather than a panic, and the envelope degrades to huma's.
			return defaultNewError(status, msg, errs...)
		}

		return &Error{ErrorModel: *base, Code: defaultCode(status)}
	}
}

/*
┌─ api ───────────────────────────────────────────
│  keeps credentials out of errors[].value
├─ in ────────────────────────────────────────────
│      ctx      huma.Context   unused
│      status   string         unused: the body's own status decides
│      v        any            the response body about to be written
├─ out ───────────────────────────────────────────
│      any      the same body, its errors[] scrubbed; never an error
├─ example ───────────────────────────────────────
│      body.password  →  no "value" at all
│      body.nested.token = "x"  →  "[redacted]"
*/

// scrubErrorValues is a huma transformer, so it runs on every error body huma writes -
// one it raised on a failed validation, one a handler returned, one built by hand with
// an ErrorDetail - and no route can forget it. See docs/api-errors.md, "What errors[].value
// holds", for the rules and for why `value` stays at all.
//
// It is a transformer and not part of useErrorEnvelope because the envelope constructor
// sees only what huma raises: a handler that returns NewError(...) with its own Errors,
// as saved views do, never passes through it.
func scrubErrorValues(_ huma.Context, _ string, v any) (any, error) {
	switch e := v.(type) {
	case *Error:
		e.Errors = scrubDetails(e.Errors)
	case *huma.ErrorModel:
		e.Errors = scrubDetails(e.Errors)
	}

	return v, nil
}

// redacted stands where a credential was inside a larger value, so the shape of what
// the client sent stays recognisable.
const redacted = "[redacted]"

// credentialWords are the fragments that make a key credential-like, matched against the
// key lower-cased with `_`, `-` and spaces removed - so password, new_password, Password,
// api-key and apiKey all hit. A fragment and not a list of exact names, because the next
// route will name its field something nobody listed. `key` alone is deliberately absent:
// a setting's key is a name, not a secret.
var credentialWords = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey", "accesskey",
	"privatekey", "credential", "authorization", "cookie", "bearer", "session",
}

func isCredentialKey(key string) bool {
	norm := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, key)

	for _, w := range credentialWords {
		if strings.Contains(norm, w) {
			return true
		}
	}

	return false
}

// locationSegments splits huma's location - body.items[0].password - into its names,
// dropping the indexes.
func locationSegments(loc string) []string {
	var out []string

	for _, part := range strings.Split(loc, ".") {
		if i := strings.IndexByte(part, '['); i >= 0 {
			part = part[:i]
		}
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

// scrubDetails returns errs with each value made safe to send. It builds new details
// and never edits the ones it was handed, which a handler may keep.
func scrubDetails(errs []*huma.ErrorDetail) []*huma.ErrorDetail {
	if errs == nil {
		return nil
	}

	out := make([]*huma.ErrorDetail, len(errs))
	for i, d := range errs {
		if d == nil {
			continue
		}

		c := *d
		c.Value = scrubDetailValue(d)
		out[i] = &c
	}

	return out
}

// scrubDetailValue decides what, if anything, of a detail's value goes out.
//
// Dropped outright:
//   - anything located in the request body, whatever its name or type. This is the rule
//     that does the work. The client just sent that value and holds a copy, so echoing it
//     tells it nothing location and message do not; and a body is where secrets are. Key
//     names cannot find them all: a password typed into the email box, a token pasted as
//     a label, a password sent as an array. huma's own values here are the offending
//     scalar, the whole enclosing object on "unexpected property", or the raw request
//     text of a body that would not parse;
//   - a header or cookie value: those are where credentials travel;
//   - a value at a location with a credential-like name wherever it is, so a query
//     parameter named token goes too.
//
// Kept: what is left is the URL - query and path values, which are in the access logs of
// every proxy already and which a caller cannot otherwise see a 422 refer to - with
// credential-like keys inside them redacted at any depth, in objects and arrays.
func scrubDetailValue(d *huma.ErrorDetail) any {
	if d.Value == nil {
		return nil
	}

	segs := locationSegments(d.Location)
	if len(segs) == 0 {
		return nil
	}

	switch strings.ToLower(segs[0]) {
	case "body", "header", "cookie":
		return nil
	}
	for _, s := range segs {
		if isCredentialKey(s) {
			return nil
		}
	}

	return scrubValue(d.Value)
}

// scrubValue redacts credential-like keys inside v. A type it does not recognise is
// round-tripped through JSON so the walk sees plain maps and slices; if that fails the
// value is dropped, since a value that cannot be inspected cannot be shown.
func scrubValue(v any) any {
	switch x := v.(type) {
	case nil, string, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if isCredentialKey(k) {
				out[k] = redacted
				continue
			}
			out[k] = scrubValue(val)
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = scrubValue(val)
		}

		return out
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	var plain any
	if err := json.Unmarshal(raw, &plain); err != nil {
		return nil
	}

	switch plain.(type) {
	case map[string]any, []any:
		return scrubValue(plain)
	}

	return plain
}

// defaultCode names a failure huma raised on its own behalf, where no handler
// was there to choose a code.
//
// It is derived from the status text rather than mapped by hand, so every
// status gets one and the table cannot fall behind net/http. That matters
// because `code` is only worth reading if it is always there - a field that is
// sometimes absent sends a client back to parsing `detail`, which is the thing
// the field exists to prevent. So Code has no omitempty.
//
//	404  →  "not_found"
//	422  →  "unprocessable_entity"
func defaultCode(status int) string {
	text := http.StatusText(status)
	if text == "" {
		return "unknown"
	}

	return strings.ToLower(strings.ReplaceAll(text, " ", "_"))
}

// writeError sends the envelope from a plain http.HandlerFunc, outside huma.
//
// notFound is the only such handler: it answers paths no huma operation
// claims, so it never reaches huma's error path and would otherwise be the one
// response in the API with a shape of its own. That is exactly the divergence
// LAM-53 exists to close.
func writeError(w http.ResponseWriter, e *Error) {
	body, err := json.Marshal(e)
	if err != nil {
		// Error holds only strings and a nil Errors slice here, so this cannot
		// fail. Falling back rather than panicking keeps a 404 a 404.
		http.Error(w, `{"status":500,"code":"internal_error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	w.Write(body)
}
