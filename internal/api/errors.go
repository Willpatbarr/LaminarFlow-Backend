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
	"net/http"
	"strings"

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
