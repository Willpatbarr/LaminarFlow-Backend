/*
╔═ hub.go ══════════════════════════════════════════════════════════════════════════════
║  realtime · in-process change feed
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Hub          struct
║      Options      struct
║      Event        struct
║      ErrClosed    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  GET /api/v1/ws · ticket writes publish after commit
║      main          →  Close on shutdown
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package realtime pushes what changed to the browsers that are watching.
//
// ADR 0005 holds the decisions; the short version is that this is one hub in one
// process, delivery is best-effort, and a client that missed something finds out by
// listing again after it reconnects. Nothing here is a source of truth: an event is a
// hint with the full row attached, never the only copy.
//
// The package knows nothing about tickets beyond the shape of an Event, and nothing
// about HTTP routing, sessions or the database. internal/api authenticates, resolves the
// caller's workspace and the ticket's workspace, and hands this package two plain
// strings. That keeps the import arrow pointing one way - api imports realtime - which is
// also why the event's ticket body is an `any`: the one mapping from a ticket to its JSON
// lives in internal/api, and copying it here would be a second definition that could
// drift from the REST shape.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ErrClosed means the hub has been shut down and takes no new connections.
var ErrClosed = errors.New("realtime: hub closed")

// The event types, which are also the `type` on the wire. Spelled once.
const (
	TicketCreated    = "ticket.created"
	TicketUpdated    = "ticket.updated"
	TicketArchived   = "ticket.archived"
	TicketUnarchived = "ticket.unarchived"

	// LAM-83. A status event carries the status in `status` and leaves the ticket
	// members off; see Event.
	StatusCreated = "status.created"
	StatusUpdated = "status.updated"
	StatusDeleted = "status.deleted"

	// LAM-85. A board event carries the board in `board`; board.column.updated carries
	// the column in `column` as well. See Event.
	BoardCreated       = "board.created"
	BoardUpdated       = "board.updated"
	BoardDeleted       = "board.deleted"
	BoardColumnUpdated = "board.column.updated"

	// LAM-93. Each carries the resource in its own member; see Event.
	EpicCreated      = "epic.created"
	EpicUpdated      = "epic.updated"
	EpicDeleted      = "epic.deleted"
	SprintCreated    = "sprint.created"
	SprintUpdated    = "sprint.updated"
	SprintDeleted    = "sprint.deleted"
	MilestoneCreated = "milestone.created"
	MilestoneUpdated = "milestone.updated"
	MilestoneDeleted = "milestone.deleted"
	LabelCreated     = "label.created"
	LabelUpdated     = "label.updated"
	LabelDeleted     = "label.deleted"

	// LAM-87. A document event carries the document in `document`; see Event.
	DocumentCreated    = "document.created"
	DocumentUpdated    = "document.updated"
	DocumentArchived   = "document.archived"
	DocumentUnarchived = "document.unarchived"

	// LAM-111. A link between a ticket and a document; carries both ids, see Event.
	TicketDocumentLinked   = "ticket.document_linked"
	TicketDocumentUnlinked = "ticket.document_unlinked"

	// LAM-99. A comment event carries the comment in `comment`; see Event.
	CommentCreated = "comment.created"
	CommentUpdated = "comment.updated"
	CommentDeleted = "comment.deleted"

	// LAM-113. A review request event carries the request in `review_request`; see Event.
	ReviewRequestCreated = "review_request.created"
	ReviewRequestUpdated = "review_request.updated"

	// LAM-89. An activity event carries the log entry in `activity`; see Event.
	ActivityCreated = "activity.created"
)

// helloFrame is the first frame on every connection. A socket that is open is not a
// socket that is working - a proxy that accepts the upgrade and then buffers shows up as
// an open connection that never delivers - so the server says something at once, and a
// client that has seen it knows the whole path works.
var helloFrame = []byte(`{"type":"hello"}`)

const (
	// Nothing a client may legitimately send is bigger than a close reason, so anything
	// larger is refused by the library before it is buffered.
	maxClientFrame = 512

	defaultSendQueue    = 64
	defaultPingInterval = 30 * time.Second
	defaultPongTimeout  = 10 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

/*
┏━ Event ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one change, as it goes over the wire
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Type        string       one of the Ticket* constants
┃      TicketID    string
┃      Version     int          the ticket's version after this change
┃      ActorID     string       the account that made it
┃      At          time.Time    the ticket's updated_at, UTC
┃      Ticket      any          the REST TicketBody, whole
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api                  one per committed write
*/

// Event carries the whole ticket so a client never needs a second request to apply it,
// archive and unarchive included. Version is repeated outside the body so a client can
// decide whether the event is news before it looks inside: it upserts when version is
// newer than its copy, and an event for its own write arrives with the version it
// already holds.
type Event struct {
	Type     string    `json:"type"`
	TicketID string    `json:"ticket_id,omitempty"`
	Version  int       `json:"version,omitempty"`
	ActorID  string    `json:"actor_id"`
	At       time.Time `json:"at"`
	Ticket   any       `json:"ticket,omitempty"`

	// TeamID, StatusID and Status are set on status.* events and absent on ticket.*
	// ones, the way the ticket members are absent on status.* ones. Status is the REST
	// StatusBody; on status.deleted it is the status as it was.
	TeamID   string `json:"team_id,omitempty"`
	StatusID string `json:"status_id,omitempty"`
	Status   any    `json:"status,omitempty"`

	// ProjectID, BoardID and Board are set on board.* events; ColumnID and Column only on
	// board.column.updated. Board is the REST BoardBody, whole and with every column; on
	// board.deleted it is the board as it was. Column is the REST ColumnBody.
	ProjectID string `json:"project_id,omitempty"`
	BoardID   string `json:"board_id,omitempty"`
	Board     any    `json:"board,omitempty"`
	ColumnID  string `json:"column_id,omitempty"`
	Column    any    `json:"column,omitempty"`

	// LAM-93. epic.*, sprint.* and milestone.* carry ProjectID and their own id and body;
	// label.* carries TeamID (above) and LabelID and Label. Each body is the REST body,
	// whole; on a *.deleted event it is the resource as it was.
	EpicID      string `json:"epic_id,omitempty"`
	Epic        any    `json:"epic,omitempty"`
	SprintID    string `json:"sprint_id,omitempty"`
	Sprint      any    `json:"sprint,omitempty"`
	MilestoneID string `json:"milestone_id,omitempty"`
	Milestone   any    `json:"milestone,omitempty"`
	LabelID     string `json:"label_id,omitempty"`
	Label       any    `json:"label,omitempty"`

	// LAM-87. document.* carries DocumentID and Document, the REST body whole, and Version
	// (above) as the ticket events do: a client applies the event only if it is newer than
	// its copy. ProjectID and TeamID (above) are set when the document is filed under one.
	// On document.archived the body is the document as archived, archived_at set.
	//
	// LAM-111. ticket.document_linked and ticket.document_unlinked carry TicketID and
	// DocumentID and nothing else of either: a link is neither's version, so there is no
	// Version and no body.
	DocumentID string `json:"document_id,omitempty"`
	Document   any    `json:"document,omitempty"`

	// LAM-99. comment.* carries CommentID and Comment, the REST CommentBody whole (the comment
	// as it was, on comment.deleted), and the comment's parent in TicketID or DocumentID
	// (exactly one). It carries no Version and no Ticket or Document: a comment has no version,
	// and the parent did not change. The event type, not the presence of ticket_id, says what
	// happened - a client must not apply a comment event as a ticket event.
	CommentID string `json:"comment_id,omitempty"`
	Comment   any    `json:"comment,omitempty"`

	// LAM-113. review_request.* carries CommentID (the request is a comment), DocumentID and
	// ReviewRequest, the REST ReviewRequestBody whole. Same rules as comment.*: no Version,
	// no Ticket or Document.
	ReviewRequest any `json:"review_request,omitempty"`

	// LAM-89. activity.created carries ActivityID and Activity, the REST ActivityBody whole, and
	// the entity it is about in TicketID or DocumentID (exactly one). It is published right
	// after the ticket.* or document.* event of the write that made the entry, one per entry,
	// and carries no Version, Ticket or Document: the entity's own event says what it looks
	// like now. An entry is immutable and never deleted by itself, so there is no
	// activity.updated or activity.deleted; entries vanish only with their ticket or document.
	ActivityID string `json:"activity_id,omitempty"`
	Activity   any    `json:"activity,omitempty"`
}

/*
┏━ Options ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the knobs a test turns; zero values are production's
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      SendQueue       int              64 frames per connection
┃      PingInterval    time.Duration    30s
┃      PongTimeout     time.Duration    10s
┃      WriteTimeout    time.Duration    10s
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      main · tests
*/

type Options struct {
	SendQueue    int
	PingInterval time.Duration
	PongTimeout  time.Duration
	WriteTimeout time.Duration
}

/*
┏━ Hub ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  every open connection, by workspace, and the fan-out to them
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      conns    map[workspace]set      guarded by mu
┃      ctx      context.Context        cancelled by Close
┃      wg       sync.WaitGroup         one per Serve in flight
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Serve(w, r, workspaceID)    →  error
┃      Publish(workspaceID, Event)
┃      Connections(workspaceID)    →  int
┃      Close()
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewHub                        one per process
*/

type Hub struct {
	opts Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.RWMutex
	closed bool
	conns  map[string]map[*conn]struct{}
}

// NewHub returns a hub with no connections. Close it: its connections hold goroutines.
func NewHub(opts Options) *Hub {
	if opts.SendQueue <= 0 {
		opts.SendQueue = defaultSendQueue
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = defaultPingInterval
	}
	if opts.PongTimeout <= 0 {
		opts.PongTimeout = defaultPongTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Hub{
		opts:   opts,
		ctx:    ctx,
		cancel: cancel,
		conns:  map[string]map[*conn]struct{}{},
	}
}

// conn is one client. send is the bounded queue between Publish, which must never
// block, and the connection's own goroutine, which may block on a slow socket as long
// as it likes - the queue is what keeps that from reaching anyone else.
type conn struct {
	workspace string
	send      chan []byte

	// dropped ends the connection from outside it. Cancelling the write context is
	// what interrupts a Write already stuck on a full socket buffer.
	cancel context.CancelFunc
	once   sync.Once
}

/*
┌─ Hub ───────────────────────────────────────────
│  upgrades to a WebSocket and serves it until it ends
├─ in ────────────────────────────────────────────
│      w              http.ResponseWriter
│      r              *http.Request
│      workspaceID    string      resolved by the caller, once
├─ out ───────────────────────────────────────────
│      error          ErrClosed · the library's refusal · nil on a clean end
├─ example ───────────────────────────────────────
│      hub closed  →  ErrClosed, nothing written
*/

// Serve blocks for the life of the connection, on the handler's own goroutine, so there
// is one handler goroutine and one reader goroutine of ours per client. The caller has
// already authenticated and checked the Origin; Accept checks the Origin again with the
// same rule, so a caller that forgot is still refused.
//
// A hijacked connection is invisible to http.Server.Shutdown, which is why Close exists
// and why main calls it first.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, workspaceID string) error {
	// Counted from the first line, not from registration, so Close cannot return
	// while a Serve is still between Accept and the registry.
	if !h.enter() {
		return ErrClosed
	}
	defer h.wg.Done()

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}

	// ctx is this connection's life. It ends when the client goes, when drop cancels it,
	// or when this function returns - never because the hub is closing, which the loop
	// below handles on its own so the close frame can still be written. (Cancelling a
	// context the library is reading or writing under closes the socket on the spot.)
	ctx, cancel := context.WithCancel(context.Background())

	// The reader. Clients send nothing in v1, but something has to read: the library
	// handles pongs and the peer's close frame from inside Read, so a connection nobody
	// reads can neither be pinged nor closed cleanly. A data frame is a client that
	// believes this is a duplex channel, and is told it is not.
	//
	// Not ws.CloseRead, which does this and has a flaw in v1.8.15: on a data frame it
	// calls Close from inside the goroutine Close then waits 15 seconds to see exit.
	ws.SetReadLimit(maxClientFrame)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()

		if _, _, err := ws.Read(ctx); err == nil {
			ws.Close(websocket.StatusPolicyViolation, "this feed is one-way")
		}
	}()

	// Serve does not return until the reader has, so a returned Serve - and therefore a
	// returned Close - means no goroutine of this connection is left.
	defer func() {
		cancel()
		ws.CloseNow()
		<-readerDone
	}()

	c := &conn{workspace: workspaceID, send: make(chan []byte, h.opts.SendQueue), cancel: cancel}

	// The hello is queued before the connection is registered, so it is always the
	// first frame and no event can overtake it.
	c.send <- helloFrame

	h.add(c)
	defer h.remove(c)

	ping := time.NewTicker(h.opts.PingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			// The client left, or drop ended it. The socket is already dead or
			// not worth a handshake; the deferred CloseNow finishes the job.
			return nil

		case <-h.ctx.Done():
			ws.Close(websocket.StatusGoingAway, "server shutting down")
			return nil

		case msg := <-c.send:
			wctx, wcancel := context.WithTimeout(ctx, h.opts.WriteTimeout)
			err := ws.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				return nil
			}

		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, h.opts.PongTimeout)
			err := ws.Ping(pctx)
			pcancel()
			if err != nil {
				// No pong inside PongTimeout: the peer is gone without having said so.
				return nil
			}
		}
	}
}

// enter admits one Serve, or refuses it once Close has started. wg.Add happens under
// the same lock Close takes to set closed, which is what makes Close's Wait complete: a
// Serve is either counted before Close looks, or refused after.
func (h *Hub) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return false
	}
	h.wg.Add(1)

	return true
}

// add registers c. A hub that closes between enter and here has already cancelled its
// context, so the connection's loop ends on its first pass and says goodbye.
func (h *Hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	set := h.conns[c.workspace]
	if set == nil {
		set = map[*conn]struct{}{}
		h.conns[c.workspace] = set
	}
	set[c] = struct{}{}
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	set := h.conns[c.workspace]
	delete(set, c)
	if len(set) == 0 {
		delete(h.conns, c.workspace)
	}
}

/*
┌─ Hub ───────────────────────────────────────────
│  sends one event to every connection in a workspace
├─ in ────────────────────────────────────────────
│      workspaceID    string
│      ev             Event
├─ example ───────────────────────────────────────
│      3 connections, 1 with a full queue  →  2 get it, 1 is dropped
*/

// Publish never blocks. A connection whose queue is full is dropped rather than waited
// for: the client reconnects and lists, which closes any gap, and one stalled tab does
// not hold up a write path or the other tabs.
//
// The event is encoded once for all recipients. An event nobody is connected for costs
// a map lookup, and the encode is skipped.
func (h *Hub) Publish(workspaceID string, ev Event) {
	h.mu.RLock()
	set := h.conns[workspaceID]
	if len(set) == 0 {
		h.mu.RUnlock()
		return
	}

	frame, err := json.Marshal(ev)
	if err != nil {
		h.mu.RUnlock()
		// An unencodable event is a bug on this side, and the client's recovery for a
		// missed event is the same as for any other. Log it and carry on.
		log.Printf("realtime: encode %s for ticket %s: %v", ev.Type, ev.TicketID, err)
		return
	}

	var slow []*conn
	for c := range set {
		select {
		case c.send <- frame:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	// Dropped outside the read lock: remove takes the write lock.
	for _, c := range slow {
		h.drop(c)
	}
}

// drop ends a connection from outside it. Cancelling its context interrupts a Write
// blocked on a full socket, which is the usual state of a slow client, and Serve's
// deferred CloseNow then tears the socket down without waiting for a peer that is not
// reading.
func (h *Hub) drop(c *conn) {
	c.once.Do(func() {
		log.Printf("realtime: dropping a slow connection in workspace %s", c.workspace)
		h.remove(c)
		c.cancel()
	})
}

// Connections is how many sockets are open in a workspace. For tests and for logs.
func (h *Hub) Connections(workspaceID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.conns[workspaceID])
}

/*
┌─ Hub ───────────────────────────────────────────
│  closes every connection and waits for them to end
├─ example ───────────────────────────────────────
│      40 open sockets  →  40 close frames, 0 goroutines left
*/

// Close is idempotent, refuses new connections from the moment it starts, and returns
// only when every Serve has returned - so "no goroutine of ours outlives the hub" is a
// property of the call, not something a caller has to poll for.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()

	h.cancel()
	h.wg.Wait()
}
