package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LAM-66. The change feed is a raw handler, so the OpenAPI-driven guards never see it:
// the first three tests stand in for them, and the rest drive it the way a browser does -
// a real HTTP server, a real WebSocket, the session cookie in the handshake.

func TestRawRoutesCarryTheVersionPrefix(t *testing.T) {
	routes := rawRoutes(nil, nil, nil)
	if len(routes) == 0 {
		t.Fatal("no raw routes, so this guard proves nothing")
	}

	found := false
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Path, V1+"/") {
			t.Errorf("raw route %s %q does not start with %q - compose it with the V1 const", rt.Method, rt.Path, V1)
		}
		found = found || rt.Path == WSPath
	}
	if !found {
		t.Errorf("the change feed is not among the raw routes")
	}

	if WSPath != "/api/v1/ws" {
		t.Errorf("WSPath = %q; the path is a published contract (docs/realtime.md)", WSPath)
	}
}

func TestEveryRawRouteRefusesAnUnauthenticatedCaller(t *testing.T) {
	mux, _ := registrationMux(t, false)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, rt := range rawRoutes(nil, nil, nil) {
		for name, header := range map[string]http.Header{
			"no credential":     {},
			"a bogus cookie":    {"Cookie": {auth.SessionCookieName + "=not-a-session"}},
			"a bogus bearer":    {"Authorization": {"Bearer lam_aaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
			"a bogus non-token": {"Authorization": {"Basic Zm9vOmJhcg=="}},
		} {
			// As a browser would send it - a real upgrade - and the refusal has to come
			// before the upgrade completes.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			c, resp, err := websocket.Dial(ctx, wsURL(srv, rt.Path), &websocket.DialOptions{HTTPHeader: header})
			cancel()

			if err == nil {
				c.CloseNow()
				t.Errorf("%s %s with %s: the upgrade succeeded", rt.Method, rt.Path, name)
				continue
			}
			if resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: got %v (%v), want 401", rt.Method, rt.Path, name, resp, err)
				continue
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
				t.Errorf("%s with %s: Content-Type = %q, want the error envelope", rt.Path, name, got)
			}
			var e Error
			if json.Unmarshal(body, &e) != nil || e.Code != "unauthorized" {
				t.Errorf("%s with %s: body %s, want code unauthorized", rt.Path, name, body)
			}
		}
	}
}

// The mux must send the route to its handler: not to the /api/ catch-all, which would
// answer 404 in the envelope, and not to the frontend, which would answer with the app
// shell. A signed-in request that is not an upgrade reaches the library, which says 426.
func TestTheFeedIsNotShadowedByTheCatchAllOrTheFrontend(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodGet, WSPath, nil, f.willC)

	if rec.Code != http.StatusUpgradeRequired {
		t.Errorf("a signed-in GET that is not an upgrade: status %d, want 426 - body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Error("GET /api/v1/ws fell through to the frontend")
	}

	// And a method the route does not take is not the feed either.
	rec = call(t, f.mux, http.MethodPost, WSPath, nil, f.willC)
	if rec.Code == http.StatusSwitchingProtocols || strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Errorf("POST /api/v1/ws answered %d", rec.Code)
	}
}

func TestACrossOriginUpgradeIsRefusedBeforeItCompletes(t *testing.T) {
	f := newFeed(t)

	for _, origin := range []string{"https://evil.example", "http://localhost:3000", "null", "not a url"} {
		header := f.cookie(f.willC)
		header.Set("Origin", origin)

		resp := f.refused(t, header)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %q: status %d, want 403", origin, resp.StatusCode)
		}
	}
}

func TestACrossOriginUpgradeWithoutACredentialIsStillRefused(t *testing.T) {
	f := newFeed(t)

	header := http.Header{"Origin": {"https://evil.example"}}
	if resp := f.refused(t, header); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want a refusal", resp.StatusCode)
	}
}

func TestACrossOriginRefusalIsInTheEnvelopeWithItsCode(t *testing.T) {
	f := newFeed(t)

	header := f.cookie(f.willC)
	header.Set("Origin", "https://evil.example")

	resp := f.refused(t, header)
	body, _ := io.ReadAll(resp.Body)

	var e Error
	if json.Unmarshal(body, &e) != nil || e.Code != CodeOriginNotAllowed {
		t.Errorf("body %s, want code %s", body, CodeOriginNotAllowed)
	}
}

// A browser on this very host sends an Origin, and it is the same host.
func TestAnUpgradeFromTheSameHostIsAccepted(t *testing.T) {
	f := newFeed(t)

	header := f.cookie(f.willC)
	header.Set("Origin", f.srv.URL)

	f.open(t, header)

	// Case does not matter in a host.
	header.Set("Origin", strings.ToUpper(f.srv.URL))
	f.open(t, header)
}

func TestABearerTokenCanOpenTheFeedToo(t *testing.T) {
	f := newFeed(t)

	token, err := auth.NewService(f.pool).Issue(context.Background(), f.will.AccountID, "script", []auth.Scope{"ticket:read"}, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	c := f.open(t, http.Header{"Authorization": {"Bearer " + token}})

	f.create(t, f.willC, "from a script's point of view")
	if got := readEvent(t, c); got.Type != "ticket.created" {
		t.Errorf("event = %+v", got)
	}
}

func TestAnAccountWithNoWorkspaceIsRefusedWithItsOwnCode(t *testing.T) {
	f := newFeed(t)

	if _, err := f.pool.Exec(context.Background(), `DELETE FROM workspace_member WHERE account_id = $1::uuid`, f.sarah.AccountID); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	resp := f.refused(t, f.cookie(f.sarahC))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}

	var e Error
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &e) != nil || e.Code != CodeNoWorkspace {
		t.Errorf("body %s, want code %s", body, CodeNoWorkspace)
	}
}

// The ticket in full: each of the four writes, in the order a client would make them,
// arrives at a second connection in the same workspace as the right event with the right
// version. The 409 in the middle is the point of the nil-write assertion: a refused write
// changes nothing and so says nothing.
func TestEachTicketWriteReachesAConnectedColleagueWithItsVersion(t *testing.T) {
	f := newFeed(t)

	sarah := f.open(t, f.cookie(f.sarahC))

	created := f.create(t, f.willC, "Fix login")
	ev := readEvent(t, sarah)
	expectEvent(t, ev, "ticket.created", created.ID, 1, f.will.AccountID)

	// A refused write publishes nothing: stale If-Match, and a project the caller cannot reach.
	if rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+created.ID, ticketPut("stale"), f.willC, "If-Match", `"7"`); rec.Code != http.StatusConflict {
		t.Fatalf("stale write: status %d, want 409", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": f.project.ID, "title": "not mine", "description": "", "status_id": nil, "assignee_id": nil,
	}, f.carolC); rec.Code != http.StatusNotFound {
		t.Fatalf("create in another workspace's project: status %d, want 404", rec.Code)
	}

	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+created.ID, ticketPut("Fix login properly"), f.sarahC, "If-Match", `"1"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d, body %s", rec.Code, rec.Body)
	}
	ev = readEvent(t, sarah)
	expectEvent(t, ev, "ticket.updated", created.ID, 2, f.sarah.AccountID)
	if ev.Ticket["title"] != "Fix login properly" {
		t.Errorf("updated event carries title %v", ev.Ticket["title"])
	}

	rec = call(t, f.mux, http.MethodDelete, V1+"/tickets/"+created.ID, nil, f.willC, "If-Match", `"2"`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("archive: status %d, body %s", rec.Code, rec.Body)
	}
	ev = readEvent(t, sarah)
	expectEvent(t, ev, "ticket.archived", created.ID, 3, f.will.AccountID)

	// The archive event is self-sufficient: the whole ticket, which no read can return.
	if ev.Ticket["title"] != "Fix login properly" || ev.Ticket["id"] != created.ID || ev.Ticket["version"] != float64(3) {
		t.Errorf("archive event's ticket = %v, want the full ticket at version 3", ev.Ticket)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/tickets/"+created.ID+"/unarchive", nil, f.sarahC, "If-Match", `"3"`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unarchive: status %d, body %s", rec.Code, rec.Body)
	}
	ev = readEvent(t, sarah)
	expectEvent(t, ev, "ticket.unarchived", created.ID, 4, f.sarah.AccountID)

	// Without If-Match, last write wins, and is announced all the same.
	if rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+created.ID, ticketPut("last word"), f.willC); rec.Code != http.StatusOK {
		t.Fatalf("blind update: status %d", rec.Code)
	}
	expectEvent(t, readEvent(t, sarah), "ticket.updated", created.ID, 5, f.will.AccountID)
}

// A client that receives its own write must be able to recognise it: the version in the
// event is the version the write's response reported.
func TestTheWritersOwnConnectionCanRecogniseItsWriteByVersion(t *testing.T) {
	f := newFeed(t)

	will := f.open(t, f.cookie(f.willC))

	created := f.create(t, f.willC, "mine")

	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+created.ID, ticketPut("mine, edited"), f.willC)
	etag := rec.Header().Get("ETag")

	expectEvent(t, readEvent(t, will), "ticket.created", created.ID, 1, f.will.AccountID)

	ev := readEvent(t, will)
	if want := strconv.Quote(strconv.Itoa(ev.Version)); etag != want {
		t.Errorf("response ETag %s, event version %d - the writer cannot tell this event is its own", etag, ev.Version)
	}
}

// One mapping, two surfaces. The event's ticket is the REST body member for member, so a
// client has one parser and the two cannot drift apart. The one difference is huma's
// "$schema" link, which it adds to a response and which belongs to the HTTP transport,
// not to TicketBody.
func TestTheEventsTicketIsTheRESTBody(t *testing.T) {
	f := newFeed(t)

	sarah := f.open(t, f.cookie(f.sarahC))

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": f.project.ID, "title": "shape", "description": "d", "status_id": nil, "assignee_id": nil,
	}, f.willC)
	var rest map[string]any
	decode(t, rec, &rest)
	delete(rest, "$schema")

	if got := readEvent(t, sarah).Ticket; !reflect.DeepEqual(got, rest) {
		t.Errorf("created: event ticket\n  %v\nREST body\n  %v", got, rest)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/tickets/"+rest["id"].(string), ticketPut("shape 2"), f.willC)
	rest = nil
	decode(t, rec, &rest)
	delete(rest, "$schema")

	if got := readEvent(t, sarah).Ticket; !reflect.DeepEqual(got, rest) {
		t.Errorf("updated: event ticket\n  %v\nREST body\n  %v", got, rest)
	}

	// Archived: no read returns it, so the keys are what is compared.
	call(t, f.mux, http.MethodDelete, V1+"/tickets/"+rest["id"].(string), nil, f.willC)
	archived := readEvent(t, sarah).Ticket

	if len(archived) != len(rest) {
		t.Errorf("archived event's ticket has %d members, REST body %d", len(archived), len(rest))
	}
	for k := range rest {
		if _, ok := archived[k]; !ok {
			t.Errorf("archived event's ticket lacks %q", k)
		}
	}
}

func TestAConnectionInAnotherWorkspaceHearsNothing(t *testing.T) {
	f := newFeed(t)

	carol := f.open(t, f.cookie(f.carolC))
	sarah := f.open(t, f.cookie(f.sarahC))

	// Every kind of write in workspace 1.
	created := f.create(t, f.willC, "private to workspace one")
	path := V1 + "/tickets/" + created.ID
	call(t, f.mux, http.MethodPut, path, ticketPut("edited"), f.willC)
	call(t, f.mux, http.MethodDelete, path, nil, f.willC)
	call(t, f.mux, http.MethodPost, path+"/unarchive", nil, f.willC)

	for i := 1; i <= 4; i++ {
		if ev := readEvent(t, sarah); ev.Version != i {
			t.Fatalf("colleague's event %d has version %d", i, ev.Version)
		}
	}

	// Carol is in workspace 2 and writes there. Frames arrive in order, so if any of the
	// four above had reached her, her first frame would be one of them.
	team := createTeam(t, f.mux, f.carolC, f.carol.WorkspaceID, "Elsewhere")
	project := createProject(t, f.mux, f.carolC, team.ID, "Other")
	rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": project.ID, "title": "hers", "description": "", "status_id": nil, "assignee_id": nil,
	}, f.carolC)
	var hers ticketWire
	decode(t, rec, &hers)

	ev := readEvent(t, carol)
	expectEvent(t, ev, "ticket.created", hers.ID, 1, f.carol.AccountID)

	// And her write did not reach workspace 1: sarah's next frame is workspace 1's next write.
	call(t, f.mux, http.MethodPut, path, ticketPut("edited again"), f.willC)
	if ev := readEvent(t, sarah); ev.TicketID != created.ID || ev.Version != 5 {
		t.Errorf("colleague's next event is %+v, want ticket %s at version 5", ev, created.ID)
	}
}

func TestEverySocketOpensWithAHelloAndNothingBeforeIt(t *testing.T) {
	f := newFeed(t)

	// A write that lands before anyone connects is not replayed: a socket is a feed
	// from now, and a client lists to learn the past.
	f.create(t, f.willC, "before")

	c := f.dial(t, f.cookie(f.sarahC))
	_, data, err := c.Read(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"hello"}` {
		t.Errorf("first frame = %s, want the hello", data)
	}
}

// ─── fixtures ─────────────────────────────────────────────────────────────

// feed is a server with three accounts: will and sarah share a workspace and a project,
// carol is alone in another.
type feed struct {
	mux                   *http.ServeMux
	pool                  *pgxpool.Pool
	srv                   *httptest.Server
	will, sarah, carol    registered
	willC, sarahC, carolC string
	project               projectWire
}

func newFeed(t *testing.T) feed {
	t.Helper()

	mux, pool := registrationMux(t, false)

	f := feed{mux: mux, pool: pool, srv: httptest.NewServer(mux)}
	t.Cleanup(f.srv.Close)

	f.will, f.willC = registerNamed(t, mux, "will@example.com", "Will Barr")
	f.sarah, f.sarahC = registerNamed(t, mux, "sarah@example.com", "Sarah Okafor")
	f.carol, f.carolC = registerNamed(t, mux, "carol@example.com", "Carol Diaz")

	// Registration joins the existing workspace (ADR 0004), so the second workspace is
	// made by hand: a new one, with carol moved into it.
	ctx := context.Background()
	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name) VALUES ('Elsewhere') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatalf("second workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_member WHERE account_id = $1::uuid`, f.carol.AccountID); err != nil {
		t.Fatalf("move carol: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_member (workspace_id, account_id, role) VALUES ($1::uuid, $2::uuid, 'owner')`, other, f.carol.AccountID); err != nil {
		t.Fatalf("move carol: %v", err)
	}
	f.carol.WorkspaceID = other

	team := createTeam(t, mux, f.willC, f.will.WorkspaceID, "Platform")
	f.project = createProject(t, mux, f.willC, team.ID, "Core")

	return f
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func (f feed) cookie(value string) http.Header {
	return http.Header{"Cookie": {auth.SessionCookieName + "=" + value}}
}

// dial opens the socket, without reading anything from it.
func (f feed) dial(t *testing.T, header http.Header) *websocket.Conn {
	t.Helper()

	c, resp, err := websocket.Dial(testCtx(t), wsURL(f.srv, WSPath), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	c.SetReadLimit(4 << 20)
	t.Cleanup(func() { c.CloseNow() })

	return c
}

// open dials and consumes the hello.
func (f feed) open(t *testing.T, header http.Header) *websocket.Conn {
	t.Helper()

	c := f.dial(t, header)
	_, data, err := c.Read(testCtx(t))
	if err != nil || string(data) != `{"type":"hello"}` {
		t.Fatalf("first frame = %s (%v), want the hello", data, err)
	}

	return c
}

// refused dials expecting the handshake to fail, and returns the response it failed with.
func (f feed) refused(t *testing.T, header http.Header) *http.Response {
	t.Helper()

	c, resp, err := websocket.Dial(testCtx(t), wsURL(f.srv, WSPath), &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		c.CloseNow()
		t.Fatal("the upgrade succeeded, want it refused")
	}
	if resp == nil {
		t.Fatalf("no response: %v", err)
	}

	return resp
}

func (f feed) create(t *testing.T, cookie, title string) ticketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": f.project.ID, "title": title, "description": "", "status_id": nil, "assignee_id": nil,
	}, cookie)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create ticket: status %d, body %s", rec.Code, rec.Body)
	}

	var out ticketWire
	decode(t, rec, &out)

	return out
}

func ticketPut(title string) map[string]any {
	return map[string]any{"title": title, "description": "d", "status_id": nil, "assignee_id": nil}
}

type eventWire struct {
	Type          string         `json:"type"`
	TicketID      string         `json:"ticket_id"`
	Version       int            `json:"version"`
	ActorID       string         `json:"actor_id"`
	At            time.Time      `json:"at"`
	Ticket        map[string]any `json:"ticket"`
	Document      map[string]any `json:"document"`
	CommentID     string         `json:"comment_id"`
	DocumentID    string         `json:"document_id"`
	Comment       map[string]any `json:"comment"`
	ReviewRequest map[string]any `json:"review_request"`
	TeamID        string         `json:"team_id"`
	StatusID      string         `json:"status_id"`

	ProjectID string         `json:"project_id"`
	BoardID   string         `json:"board_id"`
	Board     map[string]any `json:"board"`
	ColumnID  string         `json:"column_id"`
	Column    map[string]any `json:"column"`

	EpicID      string         `json:"epic_id"`
	Epic        map[string]any `json:"epic"`
	SprintID    string         `json:"sprint_id"`
	MilestoneID string         `json:"milestone_id"`
	LabelID     string         `json:"label_id"`

	ActivityID string         `json:"activity_id"`
	Activity   map[string]any `json:"activity"`
}

// readEvent is the next event a test about something else cares about: the activity.created
// events (LAM-89) that follow every ticket and document write are skipped, so the dozens of
// tests that read ticket.*, comment.* and the rest in sequence do not each have to step over
// them. activity_test.go reads them with readEventAny.
func readEvent(t *testing.T, c *websocket.Conn) eventWire {
	t.Helper()

	for {
		if ev := readEventAny(t, c); ev.Type != "activity.created" {
			return ev
		}
	}
}

func readEventAny(t *testing.T, c *websocket.Conn) eventWire {
	t.Helper()

	kind, data, err := c.Read(testCtx(t))
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("frame kind %v, want text", kind)
	}

	var ev eventWire
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("event %s: %v", data, err)
	}

	return ev
}

func expectEvent(t *testing.T, ev eventWire, kind, ticketID string, version int, actor string) {
	t.Helper()

	if ev.Type != kind || ev.TicketID != ticketID || ev.Version != version || ev.ActorID != actor {
		t.Errorf("event = {%s ticket %s v%d by %s}, want {%s ticket %s v%d by %s}",
			ev.Type, ev.TicketID, ev.Version, ev.ActorID, kind, ticketID, version, actor)
	}
	if ev.At.IsZero() {
		t.Error("event has no at")
	}
	if ev.Ticket["id"] != ticketID || ev.Ticket["version"] != float64(version) {
		t.Errorf("event's ticket = %v, want ticket %s at version %d", ev.Ticket, ticketID, version)
	}
	if ev.Ticket["updated_by"] != actor {
		t.Errorf("event's ticket.updated_by = %v, want %s", ev.Ticket["updated_by"], actor)
	}
}
