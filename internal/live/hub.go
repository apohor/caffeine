// Hub fans events out from the machine client to many browser websockets.
package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub owns the machine Client and multiplexes its events to browser clients.
type Hub struct {
	client *Client
	in     chan Event

	mu      sync.Mutex
	subs    map[chan Event]struct{}
	lastEvt map[string]Event // last event of each name for immediate replay
}

// NewHub constructs a Hub pointing at the given machine URL and starts the
// client goroutine. Call Close to tear down.
func NewHub(ctx context.Context, machineURL string) *Hub {
	in := make(chan Event, 256)
	h := &Hub{
		client:  NewClient(normaliseMachineURL(machineURL), in),
		in:      in,
		subs:    map[chan Event]struct{}{},
		lastEvt: map[string]Event{},
	}
	go h.client.Run(ctx)
	go h.pump(ctx)
	return h
}

// State returns the current machine-connection state.
func (h *Hub) State() State { return h.client.State() }

func (h *Hub) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-h.in:
			if !ok {
				return
			}
			h.mu.Lock()
			if ev.Name != "" {
				h.lastEvt[ev.Name] = ev
			}
			for c := range h.subs {
				select {
				case c <- ev:
				default:
					// subscriber slow; drop for them.
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) subscribe() (chan Event, []Event) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	// Replay the most recent of each known event so the client has
	// something to draw immediately.
	replay := make([]Event, 0, len(h.lastEvt))
	for _, ev := range h.lastEvt {
		replay = append(replay, ev)
	}
	return ch, replay
}

func (h *Hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

// Subscribe registers an in-process consumer for machine events (e.g. a
// shot recorder). Browser clients go through ServeWS; this is for Go
// callers that want a typed channel. The returned cancel func removes
// the subscription and closes the channel — always call it.
//
// Unlike the internal subscribe() used by ServeWS, this does not replay
// last-known events: an internal recorder wants a clean forward stream,
// not a retroactive re-delivery of the previous shot's final frame.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 128)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	cancel := func() { once.Do(func() { h.unsubscribe(ch) }) }
	return ch, cancel
}

// ServeWS upgrades an HTTP request to a browser WebSocket and streams events.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Safari / same-origin-deployed pages will match this naturally;
		// during `npm run dev` Vite's proxy preserves the Origin header.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("ws accept failed", "err", err.Error())
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	ctx := r.Context()
	ch, replay := h.subscribe()
	defer h.unsubscribe(ch)

	// Immediately send current state + replay of the latest events.
	if err := writeJSON(ctx, conn, map[string]any{
		"type":  "state",
		"state": h.client.State(),
	}); err != nil {
		return
	}
	for _, ev := range replay {
		if err := writeEvent(ctx, conn, ev); err != nil {
			return
		}
	}

	// Client read loop: we don't expect incoming payloads but we must read
	// to notice disconnects and respond to control frames.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Heartbeat so idle connections don't time out on load-balancers.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeEvent(ctx, conn, ev); err != nil {
				return
			}
		case <-ticker.C:
			if err := writeJSON(ctx, conn, map[string]any{"type": "ping"}); err != nil {
				return
			}
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func writeEvent(ctx context.Context, conn *websocket.Conn, ev Event) error {
	return writeJSON(ctx, conn, map[string]any{
		"type": "event",
		"name": ev.Name,
		"at":   ev.At.Format(time.RFC3339Nano),
		"data": ev.Data,
	})
}
