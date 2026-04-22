// Package live streams real-time machine events to browser clients.
//
// The Meticulous machine speaks Engine.IO v4 (the transport under socket.io).
// This package contains:
//
//   - Client: a minimal Engine.IO v4 + socket.io v4 consumer that subscribes
//     to the machine's "status" and "sensors" event streams over WebSocket.
//   - Hub: fan-out from one machine client to many connected browsers.
//
// We only need to RECEIVE events; we never emit socket.io packets back,
// so the implementation is substantially smaller than a general socket.io
// library.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is a single machine event forwarded to browser clients.
type Event struct {
	Name string          `json:"name"`           // "status" | "sensors" | "connected" | "disconnected"
	At   time.Time       `json:"at"`             // server-side receive time
	Data json.RawMessage `json:"data,omitempty"` // raw payload, as sent by the machine
}

// Client consumes events from one machine over Engine.IO v4 WebSocket.
type Client struct {
	machineURL string
	out        chan<- Event

	// connection state
	mu        sync.Mutex
	lastErr   error
	lastConn  time.Time
	connected bool
}

// NewClient returns a Client that publishes events to out. The caller owns out.
func NewClient(machineURL string, out chan<- Event) *Client {
	return &Client{machineURL: machineURL, out: out}
}

// State is the client's current observable state.
type State struct {
	Connected   bool      `json:"connected"`
	LastConnect time.Time `json:"last_connect"`
	LastError   string    `json:"last_error,omitempty"`
	MachineURL  string    `json:"machine_url"`
}

// State returns a point-in-time snapshot.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := State{
		Connected:   c.connected,
		LastConnect: c.lastConn,
		MachineURL:  c.machineURL,
	}
	if c.lastErr != nil {
		st.LastError = c.lastErr.Error()
	}
	return st
}

// Run connects and re-connects with exponential backoff until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.runOnce(ctx)
		c.setErr(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("live client disconnected", "err", err.Error())
		}
		// Backoff before retrying, capped at 30s.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err
	c.connected = false
}

func (c *Client) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	c.lastConn = time.Now().UTC()
	c.lastErr = nil
}

// runOnce performs a full handshake + read loop. Returns when the connection ends.
func (c *Client) runOnce(ctx context.Context) error {
	u, err := url.Parse(c.machineURL)
	if err != nil {
		return fmt.Errorf("parse machine url: %w", err)
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := fmt.Sprintf("%s://%s/socket.io/?EIO=4&transport=websocket", scheme, u.Host)

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	// Machine sends samples at ~10 Hz. Size the read limit generously for
	// multi-kilobyte 'sensors' frames.
	conn.SetReadLimit(1 << 20)

	// Engine.IO v4 handshake:
	//   server → client: 0{"sid":"…","pingInterval":…,"pingTimeout":…,"maxPayload":…,"upgrades":[]}
	//   client → server: 40  (socket.io CONNECT to default namespace)
	//   server → client: 40{"sid":"…"}
	// Events then arrive as "42[\"event\",{…}]".
	if err := c.expectOpen(ctx, conn); err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("40")); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}
	c.markConnected()
	c.publish(Event{Name: "connected", At: time.Now().UTC()})
	defer c.publish(Event{Name: "disconnected", At: time.Now().UTC()})

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if typ != websocket.MessageText || len(payload) == 0 {
			continue
		}
		if err := c.handleFrame(ctx, conn, payload); err != nil {
			return err
		}
	}
}

// expectOpen reads and validates the Engine.IO "0" (OPEN) handshake frame.
func (c *Client) expectOpen(ctx context.Context, conn *websocket.Conn) error {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	typ, payload, err := conn.Read(readCtx)
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}
	if typ != websocket.MessageText || len(payload) < 2 || payload[0] != '0' {
		return fmt.Errorf("unexpected handshake frame: %q", string(payload))
	}
	// We don't actually need the sid — we only receive events.
	return nil
}

// handleFrame interprets a single Engine.IO frame and forwards events.
func (c *Client) handleFrame(ctx context.Context, conn *websocket.Conn, payload []byte) error {
	// Engine.IO packet types (first byte):
	//   0 open        1 close     2 ping       3 pong
	//   4 message     5 upgrade   6 noop
	switch payload[0] {
	case '2': // ping
		return conn.Write(ctx, websocket.MessageText, []byte("3"))
	case '3': // pong (unexpected from server side; ignore)
		return nil
	case '4': // socket.io-over-engine.io message
		return c.handleSocketIO(payload[1:])
	default:
		// 0/1/5/6 — ignore for our read-only client.
		return nil
	}
}

// handleSocketIO parses the socket.io packet body (after the Engine.IO '4').
//
// Packet types we care about:
//
//	0 CONNECT    (server ack — payload "{\"sid\":\"…\"}")
//	2 EVENT      (payload starts with '[' and contains ["name", data, ...])
//	4 ERROR
func (c *Client) handleSocketIO(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	switch body[0] {
	case '0':
		// CONNECT ack — nothing to do.
		return nil
	case '2':
		return c.dispatchEvent(body[1:])
	default:
		return nil
	}
}

// dispatchEvent turns `["name",{…}]` into an Event on the output channel.
func (c *Client) dispatchEvent(body []byte) error {
	// The machine namespace is the default, so there is no "/namespace,"
	// prefix and no ack id. body looks like `["status",{...}]`.
	trim := body
	// Skip optional ack id digits after the packet-type we already consumed.
	for len(trim) > 0 && trim[0] >= '0' && trim[0] <= '9' {
		trim = trim[1:]
	}
	if len(trim) == 0 || trim[0] != '[' {
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(trim, &parts); err != nil {
		return nil // malformed; skip
	}
	if len(parts) < 1 {
		return nil
	}
	var name string
	if err := json.Unmarshal(parts[0], &name); err != nil {
		return nil
	}
	var data json.RawMessage
	if len(parts) > 1 {
		data = parts[1]
	}
	// Only forward names we know the UI cares about. Keeps noise low and
	// shields the browser from private/internal events.
	if !allowedEvent(name) {
		return nil
	}
	c.publish(Event{Name: name, At: time.Now().UTC(), Data: data})
	return nil
}

func allowedEvent(name string) bool {
	switch name {
	case "status", "sensors":
		return true
	default:
		return false
	}
}

func (c *Client) publish(ev Event) {
	// Drop the event if the hub is not keeping up rather than blocking the
	// read loop. The UI only needs near-real-time, not every frame.
	select {
	case c.out <- ev:
	default:
	}
}

// --- helpers ---------------------------------------------------------------

// normaliseMachineURL trims trailing slashes and ensures a scheme.
func normaliseMachineURL(raw string) string {
	raw = strings.TrimRight(raw, "/")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw
}
