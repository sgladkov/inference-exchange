// Package hub keeps the open connections to providers and dispatches jobs over them.
//
// Providers dial in and never listen on a port, which is what lets a laptop behind NAT sell. The
// registry is the only publicly reachable component in the system.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var (
	ErrOffline  = errors.New("provider is not connected")
	ErrBusy     = errors.New("provider already has this job in flight")
	ErrHangup   = errors.New("provider disconnected before returning a result")
	ErrTimedOut = errors.New("provider did not return a result in time")
)

// Frames on the wire. One job at a time per provider keeps this a request/response protocol with a
// correlation id rather than a pipeline needing flow control.
const (
	FrameJob       = "job"       // registry → provider: do this
	FrameAccepted  = "accepted"  // provider → registry: picked it up
	FrameResult    = "result"    // provider → registry: done, with usage
	FrameFailed    = "failed"    // provider → registry: could not do it
	FrameHeartbeat = "heartbeat" // either way
)

// Frame is every message in both directions.
type Frame struct {
	Type   string `json:"type"`
	JobID  string `json:"job_id,omitempty"`
	Prompt string `json:"prompt,omitempty"`

	// MaxUnits travels to the provider so it can refuse or truncate work it cannot do within the
	// buyer's ceiling. It is advisory there — the ceiling is enforced by the registry at pricing
	// time, which the provider does not control.
	MaxUnits int64 `json:"max_units,omitempty"`

	Result string `json:"result,omitempty"`
	Units  int64  `json:"units,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Outcome is what a dispatch produced.
type Outcome struct {
	Result string
	Units  int64
	Err    error
}

type conn struct {
	ws      *websocket.Conn
	mu      sync.Mutex // serialises writes; the websocket library allows one writer at a time
	pending map[string]chan Outcome
	pendMu  sync.Mutex
}

// Hub owns provider connections.
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*conn
	log   *slog.Logger

	// OnStateChange is called when a provider connects or disconnects, so the store can track
	// online status without the hub importing it.
	OnStateChange func(providerID string, online bool)
}

func New(log *slog.Logger) *Hub {
	return &Hub{conns: map[string]*conn{}, log: log}
}

// Online reports whether a provider currently holds a connection.
func (h *Hub) Online(providerID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[providerID]
	return ok
}

// Serve takes over an accepted websocket and blocks until the provider disconnects.
//
// A second connection for the same provider replaces the first: a provider that restarts should be
// able to reconnect without waiting for the old socket to time out. Anything the old connection
// still had in flight is failed rather than left hanging.
func (h *Hub) Serve(ctx context.Context, providerID string, ws *websocket.Conn) {
	c := &conn{ws: ws, pending: map[string]chan Outcome{}}

	h.mu.Lock()
	if old, ok := h.conns[providerID]; ok {
		h.log.Warn("provider reconnected, replacing existing connection", "provider", providerID)
		old.failAll(ErrHangup)
		old.ws.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	h.conns[providerID] = c
	h.mu.Unlock()

	if h.OnStateChange != nil {
		h.OnStateChange(providerID, true)
	}
	h.log.Info("provider online", "provider", providerID)

	defer func() {
		h.mu.Lock()
		// Only tear down the registration if it is still ours. A replacement connection may have
		// taken over already, and marking the provider offline then would be wrong.
		if cur, ok := h.conns[providerID]; ok && cur == c {
			delete(h.conns, providerID)
			if h.OnStateChange != nil {
				h.OnStateChange(providerID, false)
			}
			h.log.Info("provider offline", "provider", providerID)
		}
		h.mu.Unlock()
		c.failAll(ErrHangup)
	}()

	for {
		var f Frame
		if err := wsjson.Read(ctx, ws, &f); err != nil {
			return // disconnect, timeout, or context cancellation — all end the session
		}
		switch f.Type {
		case FrameResult:
			c.resolve(f.JobID, Outcome{Result: f.Result, Units: f.Units})
		case FrameFailed:
			c.resolve(f.JobID, Outcome{Err: fmt.Errorf("provider failed the job: %s", f.Error)})
		case FrameAccepted, FrameHeartbeat:
			// nothing to do; reading it is the liveness signal
		default:
			h.log.Warn("unknown frame from provider", "provider", providerID, "type", f.Type)
		}
	}
}

// Dispatch sends a job and waits for the provider's result.
//
// The caller's context bounds the wait, and it can be long: no Hedera transaction is in flight
// while a job runs, so a job may take minutes without any payment expiring.
func (h *Hub) Dispatch(ctx context.Context, providerID, jobID, prompt string, maxUnits int64) (Outcome, error) {
	h.mu.RLock()
	c, ok := h.conns[providerID]
	h.mu.RUnlock()
	if !ok {
		return Outcome{}, ErrOffline
	}

	ch := make(chan Outcome, 1)
	c.pendMu.Lock()
	if _, dup := c.pending[jobID]; dup {
		c.pendMu.Unlock()
		return Outcome{}, ErrBusy
	}
	c.pending[jobID] = ch
	c.pendMu.Unlock()

	defer func() {
		c.pendMu.Lock()
		delete(c.pending, jobID)
		c.pendMu.Unlock()
	}()

	c.mu.Lock()
	err := wsjson.Write(ctx, c.ws, Frame{
		Type: FrameJob, JobID: jobID, Prompt: prompt, MaxUnits: maxUnits,
	})
	c.mu.Unlock()
	if err != nil {
		return Outcome{}, fmt.Errorf("dispatch to %s: %w", providerID, err)
	}

	select {
	case out := <-ch:
		return out, out.Err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Outcome{}, ErrTimedOut
		}
		return Outcome{}, ctx.Err()
	}
}

func (c *conn) resolve(jobID string, out Outcome) {
	c.pendMu.Lock()
	ch, ok := c.pending[jobID]
	if ok {
		delete(c.pending, jobID)
	}
	c.pendMu.Unlock()
	if ok {
		ch <- out
	}
}

// failAll releases every waiter on this connection. Called when the socket goes away, so a
// disconnect surfaces as a distinct error immediately rather than as a timeout minutes later.
func (c *conn) failAll(err error) {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = map[string]chan Outcome{}
	c.pendMu.Unlock()
	for _, ch := range pending {
		ch <- Outcome{Err: err}
	}
}

// Heartbeat pings connected providers so dead sockets are noticed rather than lingering.
func (h *Hub) Heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.RLock()
			conns := make(map[string]*conn, len(h.conns))
			for id, c := range h.conns {
				conns[id] = c
			}
			h.mu.RUnlock()
			for id, c := range conns {
				pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := c.ws.Ping(pingCtx); err != nil {
					h.log.Warn("provider failed ping, closing", "provider", id, "err", err)
					c.ws.Close(websocket.StatusGoingAway, "ping timeout")
				}
				cancel()
			}
		}
	}
}

// MarshalFrame is exposed so the provider daemon's protocol can be exercised from tests.
func MarshalFrame(f Frame) ([]byte, error) { return json.Marshal(f) }
