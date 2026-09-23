package mm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

// WSEvent is one server-pushed websocket event.
type WSEvent struct {
	Event string                     `json:"event"`
	Data  map[string]json.RawMessage `json:"data"`
	Seq   int64                      `json:"seq"`
}

// WSConn is an authenticated websocket connection to the server.
type WSConn struct {
	conn *websocket.Conn
}

// DialWS opens the event websocket. A non-empty connID with nextSeq asks the
// server to resume that connection and replay events from nextSeq.
func (c *Client) DialWS(ctx context.Context, connID string, nextSeq int64) (*WSConn, error) {
	u, err := url.Parse(c.baseURL + "/api/v4/websocket")
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	if connID != "" {
		q := u.Query()
		q.Set("connection_id", connID)
		q.Set("sequence_number", strconv.FormatInt(nextSeq, 10))
		u.RawQuery = q.Encode()
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	// Channel messages can exceed the 32KB default.
	conn.SetReadLimit(4 << 20)
	return &WSConn{conn: conn}, nil
}

// Next blocks until the next event. Replies to client requests are skipped.
func (w *WSConn) Next(ctx context.Context) (*WSEvent, error) {
	for {
		_, raw, err := w.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var ev WSEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, fmt.Errorf("decode websocket event: %w", err)
		}
		if ev.Event != "" {
			return &ev, nil
		}
	}
}

// Ping sends a ping and waits for the pong; it needs a concurrent Next.
func (w *WSConn) Ping(ctx context.Context) error {
	return w.conn.Ping(ctx)
}

// CloseNow drops the connection without a close handshake, which could
// otherwise stall on a dead peer.
func (w *WSConn) CloseNow() error {
	return w.conn.CloseNow()
}
