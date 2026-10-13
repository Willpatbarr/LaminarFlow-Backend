package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// LAM-66. The hub against a real HTTP server and a real WebSocket client: nothing here
// fakes a connection, because the behaviours under test - a close frame, a full socket
// buffer, a missing pong - exist only on a real one. The handler stands in for what
// internal/api does around Serve (authenticate, resolve the workspace) by reading the
// workspace from the query, so these tests need no database.

func newHub(t *testing.T, opts Options) (*Hub, *httptest.Server) {
	t.Helper()

	hub := NewHub(opts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Serve(w, r, r.URL.Query().Get("ws")); errors.Is(err, ErrClosed) {
			http.Error(w, "closed", http.StatusServiceUnavailable)
		}
	}))

	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	return hub, srv
}

func dial(t *testing.T, srv *httptest.Server, workspace string) *websocket.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/?ws="+workspace, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Events carry a whole ticket; the library's default 32 KiB read limit is for a
	// protocol that sends less.
	c.SetReadLimit(4 << 20)
	t.Cleanup(func() { c.CloseNow() })

	return c
}

func read(t *testing.T, c *websocket.Conn, within time.Duration) (map[string]any, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()

	_, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}

	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("frame %q is not JSON: %v", data, err)
	}

	return frame, nil
}

func mustRead(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()

	frame, err := read(t, c, 5*time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return frame
}

// connected dials and consumes the hello, so the caller starts from a working socket.
func connected(t *testing.T, srv *httptest.Server, workspace string) *websocket.Conn {
	t.Helper()

	c := dial(t, srv, workspace)
	if hello := mustRead(t, c); hello["type"] != "hello" {
		t.Fatalf("first frame = %v, want hello", hello)
	}

	return c
}

func eventAt(version int) Event {
	return Event{
		Type: TicketUpdated, TicketID: "t-1", Version: version, ActorID: "a-1",
		At: time.Date(2026, 10, 13, 17, 2, 11, 0, time.UTC), Ticket: map[string]any{"title": "x"},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheFirstFrameIsHelloAndNothingElse(t *testing.T) {
	_, srv := newHub(t, Options{})

	c := dial(t, srv, "w1")

	hello := mustRead(t, c)
	if len(hello) != 1 || hello["type"] != "hello" {
		t.Errorf("first frame = %v, want exactly {type: hello}", hello)
	}
}

func TestAnEventIsOneTextFrameWithTheDocumentedMembers(t *testing.T) {
	hub, srv := newHub(t, Options{})
	c := connected(t, srv, "w1")

	hub.Publish("w1", eventAt(7))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kind, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.MessageText {
		t.Errorf("frame kind = %v, want text", kind)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}

	for _, key := range []string{"type", "ticket_id", "version", "actor_id", "at", "ticket"} {
		if _, ok := got[key]; !ok {
			t.Errorf("event has no %q: %s", key, data)
		}
	}
	if len(got) != 6 {
		t.Errorf("event has %d members, want exactly 6: %s", len(got), data)
	}
	if got["type"] != "ticket.updated" || got["version"] != float64(7) || got["at"] != "2026-10-13T17:02:11Z" {
		t.Errorf("event = %s", data)
	}
}

func TestAnEventReachesEveryConnectionInItsWorkspaceAndNoOther(t *testing.T) {
	hub, srv := newHub(t, Options{})

	a1, a2 := connected(t, srv, "a"), connected(t, srv, "a")
	b := connected(t, srv, "b")

	hub.Publish("a", eventAt(2))

	for i, c := range []*websocket.Conn{a1, a2} {
		if got := mustRead(t, c); got["version"] != float64(2) {
			t.Errorf("connection %d of workspace a got %v, want version 2", i, got)
		}
	}

	// A read that waits for nothing would close the socket when it timed out, so the
	// absence is shown the other way: publish to b next, and b's first frame must be
	// that one. Frames arrive in order, so had the event for a reached b it would be
	// ahead of this.
	hub.Publish("b", eventAt(3))
	if got := mustRead(t, b); got["version"] != float64(3) {
		t.Errorf("workspace b's first event is %v, want version 3 - it was sent the event for workspace a", got)
	}
}

func TestEventsArriveInTheOrderTheyWerePublished(t *testing.T) {
	hub, srv := newHub(t, Options{})
	c := connected(t, srv, "w1")

	for v := 1; v <= 20; v++ {
		hub.Publish("w1", eventAt(v))
	}

	for v := 1; v <= 20; v++ {
		if got := mustRead(t, c); got["version"] != float64(v) {
			t.Fatalf("frame %d has version %v", v, got["version"])
		}
	}
}

func TestPublishingToAWorkspaceNobodyWatchesIsHarmless(t *testing.T) {
	hub, _ := newHub(t, Options{})

	hub.Publish("nobody", eventAt(1))

	hub.Close()
	hub.Publish("nobody", eventAt(2))
}

// The ticket's own words: a slow client is dropped, and does not block the others. The
// slow connection never reads, so its socket buffers fill and then its queue does. The
// publisher paces itself to the fast client - otherwise the fast client's own queue
// would be what overflowed, which is the same rule applied to a client that was merely
// behind.
func TestASlowConsumerIsDroppedAndNeverBlocksAFastOne(t *testing.T) {
	hub, srv := newHub(t, Options{SendQueue: 4})

	fast := connected(t, srv, "w1")
	slow := connected(t, srv, "w1")

	if n := hub.Connections("w1"); n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}

	big := Event{Type: TicketUpdated, TicketID: "t-1", ActorID: "a-1", At: time.Now(),
		Ticket: map[string]any{"description": strings.Repeat("x", 256<<10)}}

	var worst time.Duration
	var published int

	for v := 1; v <= 600 && hub.Connections("w1") == 2; v++ {
		big.Version = v

		start := time.Now()
		hub.Publish("w1", big)
		if d := time.Since(start); d > worst {
			worst = d
		}
		published = v

		if got := mustRead(t, fast); got["version"] != float64(v) {
			t.Fatalf("fast client got version %v, want %d", got["version"], v)
		}
	}

	if hub.Connections("w1") != 1 {
		t.Fatalf("the slow connection was never dropped after %d events of 256 KiB", published)
	}
	if worst > time.Second {
		t.Errorf("a Publish took %v, so a slow client held up the publisher", worst)
	}

	// The fast client is still served after the drop.
	big.Version = published + 1
	hub.Publish("w1", big)
	if got := mustRead(t, fast); got["version"] != float64(published+1) {
		t.Errorf("fast client after the drop got version %v", got["version"])
	}

	// The slow client's socket is closed: draining what was buffered ends in an error.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := read(t, slow, 5*time.Second); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dropped client's socket never ended")
		}
	}
}

func TestClosingTheHubClosesEveryConnectionAndLeavesNoGoroutines(t *testing.T) {
	hub := NewHub(Options{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Serve(w, r, r.URL.Query().Get("ws")); errors.Is(err, ErrClosed) {
			http.Error(w, "closed", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	baseline := runtime.NumGoroutine()

	var clients []*websocket.Conn
	for i := 0; i < 8; i++ {
		clients = append(clients, connected(t, srv, "w1"))
	}
	if n := hub.Connections("w1"); n != 8 {
		t.Fatalf("%d connections, want 8", n)
	}

	// The clients read while the hub closes: a close handshake needs the peer to answer.
	statuses := make(chan websocket.StatusCode, len(clients))
	for _, c := range clients {
		go func() {
			_, err := read(t, c, 10*time.Second)
			statuses <- websocket.CloseStatus(err)
		}()
	}

	closed := make(chan struct{})
	go func() {
		hub.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: a connection goroutine is stuck")
	}

	if n := hub.Connections("w1"); n != 0 {
		t.Errorf("%d connections after Close, want 0", n)
	}

	for range clients {
		if got := <-statuses; got != websocket.StatusGoingAway {
			t.Errorf("a client ended with close status %d, want %d (going away)", got, websocket.StatusGoingAway)
		}
	}
	for _, c := range clients {
		c.CloseNow()
	}

	// Close returned, so Serve has: nothing of the hub's remains. What is left to settle
	// is the test's own clients and the server's connection bookkeeping.
	srv.CloseClientConnections()
	waitFor(t, "goroutines to return to the baseline", func() bool {
		return runtime.NumGoroutine() <= baseline
	})

	// A hub that is closed takes no more.
	resp, err := http.Get(srv.URL + "/?ws=w1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a closed hub answered %d, want 503", resp.StatusCode)
	}
}

func TestCloseIsIdempotentAndWithNoConnectionsReturnsAtOnce(t *testing.T) {
	hub := NewHub(Options{})

	hub.Close()
	hub.Close()
}

func TestAClientThatStopsAnsweringPingsIsReaped(t *testing.T) {
	hub, srv := newHub(t, Options{PingInterval: 50 * time.Millisecond, PongTimeout: 100 * time.Millisecond})

	// The library answers a ping only from inside Read. This client never calls Read
	// after the handshake, which is what a frozen tab or a pulled cable looks like to
	// the server.
	dial(t, srv, "w1")

	waitFor(t, "the registration", func() bool { return hub.Connections("w1") == 1 })
	waitFor(t, "the silent connection to be reaped", func() bool { return hub.Connections("w1") == 0 })
}

func TestAClientThatAnswersPingsIsKept(t *testing.T) {
	hub, srv := newHub(t, Options{PingInterval: 30 * time.Millisecond, PongTimeout: 500 * time.Millisecond})

	c := connected(t, srv, "w1")

	// Reading is what answers pings, so a reader runs while several intervals pass. It
	// is cancelled at the end, which closes the socket - hence the check comes first.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Read(ctx)

	time.Sleep(400 * time.Millisecond)

	if n := hub.Connections("w1"); n != 1 {
		t.Errorf("%d connections after about thirteen pings, want 1", n)
	}
}

func TestADisconnectedClientIsForgotten(t *testing.T) {
	hub, srv := newHub(t, Options{})

	c := connected(t, srv, "w1")
	c.Close(websocket.StatusNormalClosure, "bye")

	waitFor(t, "the connection to leave the hub", func() bool { return hub.Connections("w1") == 0 })
}

// The feed is one-way. A client that sends data is not being listened to, and is told so
// rather than ignored, so a client written to expect a reply finds out in development.
func TestAClientThatSendsDataIsClosed(t *testing.T) {
	hub, srv := newHub(t, Options{})

	c := connected(t, srv, "w1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello?"}`))

	_, err := read(t, c, 5*time.Second)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close status = %d (%v), want policy violation", got, err)
	}

	waitFor(t, "the connection to leave the hub", func() bool { return hub.Connections("w1") == 0 })
}
