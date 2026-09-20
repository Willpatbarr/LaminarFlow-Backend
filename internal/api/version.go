/*
╔═ version.go ═══════════════════════════════════════════════════════════════════════════
║  api contract · version prefix
╠═ declares ═════════════════════════════════════════════════════════════════════════════
║      V1             the path prefix every v1 route composes with
║      SpecVersion    the OpenAPI info.version
╠═ reached from ═════════════════════════════════════════════════════════════════════════
║      NewHumaAPI    →  docs, openapi and schema paths
║      registerPing  →  /api/v1/ping
╚════════════════════════════════════════════════════════════════════════════════════════
*/

package api

// V1 is the path prefix of version 1 of this API, and the only place it is
// spelled.
//
// LAM-54 settled what the segment promises. Within v1 this API adds and never
// takes away: a new response field, a new endpoint, and a new optional request
// field all ship freely, and a client is expected to ignore fields it does not
// know. Four things are not allowed without a new version:
//
//	removing or renaming a field
//	removing an endpoint
//	tightening validation on an existing field
//	moving a case to a different status code
//
// LAM-53 is the worked example. It added `code` to every error response, which
// is additive, so it did not need v2 - even though it changed what every error
// looks like on the wire.
//
// Who this is for. The web app cannot suffer version skew: internal/frontend
// serves the bundle from this same binary, so it always talks to the API it
// was built against. The promise exists for the other caller in API Design
// notes section 5 - scripts and agents holding API tokens, which upgrade on
// their own schedule or never.
//
// When v2 arrives, v1 keeps answering for a stated window and is then removed.
// Both run side by side in the meantime, which is why the OpenAPI document is
// served per version rather than once at the root - see NewHumaAPI.
//
// A route never writes this prefix as a literal. TestEveryRouteCarriesThe
// VersionPrefix fails the build if one does, because a typo'd prefix is a 404
// that looks exactly like every other 404.
const V1 = "/api/v1"

// SpecVersion is the OpenAPI document's info.version.
//
// The major always equals the digit in V1, so the two can never disagree -
// which they did before LAM-54, when the path said v1 and the document said
// 0.1.0. That pairing told a reader both "stable contract" and "pre-1.0, no
// promises" at once.
//
// The minor moves on each additive change, since additive is exactly what v1
// permits. The patch moves on a fix that changes no shape at all.
//
//	/api/v1  ->  1.x.y
//	/api/v2  ->  2.x.y
const SpecVersion = "1.0.0"
