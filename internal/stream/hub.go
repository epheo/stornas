// Package stream pushes live snapshots to WebSocket clients. One central
// goroutine reconciles the shared frame to the latest change-version and
// hands every connection the freshest bytes; connections are level-triggered
// writers. No debounce (coalescing falls out of the build duration), no
// heartbeat broadcast (a fresh connection gets a frame on attach; a quiet
// one needs no resend), no send-buffer overflow (each connection's mailbox
// conflates to the latest frame, so a slow client converges instead of
// dropping). Same design as dotvirt's hub minus per-identity frames: stornas
// has one shared snapshot until the auth layer lands.
package stream

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/epheo/stornas/internal/model"
)

// Hub is the central snapshot reconciler. One Hub per process.
type Hub struct {
	snapshot func() model.Snapshot
	wake     <-chan struct{} // bus subscription over the snapshot kinds (the edge)
	version  func() uint64   // summed version of those kinds (the level)
	kick     chan struct{}   // a connection was added - rebuild so it gets a first frame

	mu    sync.Mutex
	conns map[*conn]struct{}
}

// NewHub builds the hub over a bus subscription (wake) and a reader for the
// summed version of the kinds that subscription covers. Passing both keeps
// this package decoupled from the specific kind set - the caller owns it.
func NewHub(snapshot func() model.Snapshot, wake <-chan struct{}, version func() uint64) *Hub {
	return &Hub{
		snapshot: snapshot,
		wake:     wake,
		version:  version,
		kick:     make(chan struct{}, 1),
		conns:    map[*conn]struct{}{},
	}
}

// conn is one WebSocket connection. Run is the sole writer of last and the
// sole producer into out (a conflating 1-slot mailbox); writePump drains out
// and writes the latest frame. quit is closed on teardown.
type conn struct {
	out  chan []byte
	quit chan struct{}
	last string
}

// frame is the wire envelope; versioned so the client can grow without
// guessing at payload shape.
type frame struct {
	Snapshot model.Snapshot `json:"snapshot"`
}

// Run reconciles the shared frame whenever the bus wakes it or a connection
// attaches, re-checking the version after each build so a change landing
// mid-build triggers another pass instead of being lost.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-h.kick:
		}
		for {
			v := h.version()
			js, err := json.Marshal(frame{Snapshot: h.snapshot()})
			if err != nil {
				log.Printf("stream: encode frame: %v", err)
				break
			}
			h.deliver(js)
			if h.version() == v {
				break
			}
		}
	}
}

func (h *Hub) deliver(js []byte) {
	s := string(js)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.last == s {
			continue
		}
		c.last = s
		// Conflate: drop a stale undelivered frame, then queue the new one.
		select {
		case <-c.out:
		default:
		}
		select {
		case c.out <- js:
		case <-c.quit:
		}
	}
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	close(c.quit)
}

var upgrader = websocket.Upgrader{CheckOrigin: checkOrigin}

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client (no Origin header)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// ServeHTTP upgrades the request and streams frames until the client goes
// away. The read loop exists only to notice the close.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{out: make(chan []byte, 1), quit: make(chan struct{})}
	h.add(c)
	go func() {
		defer func() { _ = ws.Close() }()
		for {
			select {
			case js := <-c.out:
				if err := ws.WriteMessage(websocket.TextMessage, js); err != nil {
					return
				}
			case <-c.quit:
				return
			}
		}
	}()
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			break
		}
	}
	h.remove(c)
}
