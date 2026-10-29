#!/bin/sh
# The README quickstart as a script: everything an API client does, with only a token.
#
#   LF_TOKEN=lam_... ./scripts/agent-quickstart.sh            # against http://localhost:8080
#   LF_TOKEN=lam_... LF_URL=https://lf.example ./scripts/agent-quickstart.sh
#
# The token needs ticket:read, ticket:write, project:read and project:write, plus team:read and
# team:write when the workspace has no team yet (create one in the app under Settings, or with
# POST /api/v1/auth/tokens from a session - a token cannot mint tokens).
# Every call below sends `Authorization: Bearer` and nothing else for identity; there is no
# cookie jar. CI runs this against a fresh database (.github/workflows/ci.yml).
#
# It creates one ticket and one document, edits both, asks for a review, and archives both
# again on the way out, so it is safe to point at a real workspace. Needs curl and jq.

set -eu

: "${LF_TOKEN:?set LF_TOKEN to an API token (lam_...)}"
BASE="${LF_URL:-http://localhost:8080}/api/v1"
BODY=$(mktemp)
HEADERS=$(mktemp)
TICKET_ID=""
DOC_ID=""

# call METHOD PATH [JSON [extra curl args...]] - prints the response body, fails on non-2xx.
call() {
    method=$1 path=$2 data=${3:-}
    [ $# -ge 3 ] && shift 3 || shift 2
    if [ -n "$data" ]; then
        code=$(curl -sS -o "$BODY" -D "$HEADERS" -w '%{http_code}' -X "$method" "$BASE$path" \
            -H "Authorization: Bearer $LF_TOKEN" -H 'Content-Type: application/json' -d "$data" "$@")
    else
        code=$(curl -sS -o "$BODY" -D "$HEADERS" -w '%{http_code}' -X "$method" "$BASE$path" \
            -H "Authorization: Bearer $LF_TOKEN" "$@")
    fi
    case "$code" in
    2??) cat "$BODY" ;;
    *)
        echo "x $method $path -> $code" >&2
        cat "$BODY" >&2
        echo >&2
        exit 1
        ;;
    esac
}

# etag - the quoted version of the last response, for If-Match.
etag() { tr -d '\r' <"$HEADERS" | sed -n 's/^[Ee][Tt][Aa][Gg]: *//p'; }

step() { echo "==> $*"; }

cleanup() {
    # Best effort, and silent: a failed run should show its own error, not this one's.
    [ -n "$TICKET_ID" ] && curl -s -o /dev/null -X DELETE "$BASE/tickets/$TICKET_ID" -H "Authorization: Bearer $LF_TOKEN" || true
    [ -n "$DOC_ID" ] && curl -s -o /dev/null -X DELETE "$BASE/documents/$DOC_ID" -H "Authorization: Bearer $LF_TOKEN" || true
    rm -f "$BODY" "$HEADERS"
}
trap cleanup EXIT

step "who am I (GET /auth/me)"
me=$(call GET /auth/me)
echo "$me" | jq -e '.via == "token"' >/dev/null || { echo "x not authenticated by a token" >&2; exit 1; }
account_id=$(echo "$me" | jq -r .account_id)
echo "$me" | jq -c '{via, scopes, permissions}'

step "pick a project (POST /projects/list), creating a team and project in an empty workspace"
project_id=$(call POST /projects/list '{"limit":1}' | jq -r '.items[0].id // empty')
if [ -z "$project_id" ]; then
    team_id=$(call POST /teams/list '{"limit":1}' | jq -r '.items[0].id // empty')
    if [ -z "$team_id" ]; then
        workspace_id=$(echo "$me" | jq -er .workspace_id)
        team_id=$(call POST /teams "$(jq -nc --arg w "$workspace_id" '{workspace_id: $w, name: "Quickstart team"}')" | jq -er .id)
    fi
    project_id=$(call POST /projects "$(jq -nc --arg t "$team_id" '{team_id: $t, name: "Quickstart project"}')" | jq -er .id)
fi
echo "project $project_id"

step "list tickets (POST /tickets/list)"
call POST /tickets/list '{"limit":5}' | jq -c '{count: (.items | length), has_more}'

step "create a ticket (POST /tickets)"
key="quickstart-ticket-$$-$(date +%s)"
ticket=$(call POST /tickets "$(jq -nc --arg p "$project_id" '{project_id: $p, title: "Quickstart ticket", description: "made by agent-quickstart.sh"}')" -H "Idempotency-Key: $key")
TICKET_ID=$(echo "$ticket" | jq -er .id)
echo "ticket $TICKET_ID"

step "update the ticket (PUT /tickets/{id}, If-Match)"
version=$(etag)
update=$(echo "$ticket" | jq -c '{title: "Quickstart ticket, edited", description, status_id, assignee_id}')
call PUT "/tickets/$TICKET_ID" "$update" -H "If-Match: $version" | jq -c '{title, version}'

step "list documents (POST /documents/list)"
call POST /documents/list '{"limit":5}' | jq -c '{count: (.items | length), has_more}'

step "create a document (POST /documents)"
key="quickstart-doc-$$-$(date +%s)"
doc=$(call POST /documents "$(jq -nc --arg p "$project_id" '{type: "normal", title: "Quickstart document", project_id: $p, body: {content: "Hello from an API client."}}')" -H "Idempotency-Key: $key")
DOC_ID=$(echo "$doc" | jq -er .id)
echo "document $DOC_ID"

step "update the document (PUT /documents/{id}, If-Match)"
version=$(etag)
update=$(echo "$doc" | jq -c '{type, title: "Quickstart document, edited", aspect_type_id, body: {content: "Hello again from an API client."}}')
call PUT "/documents/$DOC_ID" "$update" -H "If-Match: $version" | jq -c '{title, version}'

step "link the document to the ticket (POST /tickets/{id}/documents)"
call POST "/tickets/$TICKET_ID/documents" "$(jq -nc --arg d "$DOC_ID" '{document_id: $d}')" >/dev/null
call GET "/tickets/$TICKET_ID" | jq -e --arg d "$DOC_ID" '.documents | map(.id) | index($d) != null' >/dev/null

step "ask for a review of the first five characters (POST /review-requests)"
key="quickstart-review-$$-$(date +%s)"
call POST /review-requests "$(jq -nc --arg d "$DOC_ID" --arg r "$account_id" \
    '{document_id: $d, region_start: 0, region_end: 5, body: "Does this opening work?", reviewer_ids: [$r]}')" \
    -H "Idempotency-Key: $key" | jq -c '{id, review_status, anchor_text}'

echo "ok: quickstart finished"
