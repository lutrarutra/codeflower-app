// Package attach opens a sandbox chat in the agent's own terminal UI
// (`codeflower attach`): the UI runs in the sandbox, in a terminal the bridge
// keeps (`_codeflower/terminal/*`), and this side relays the user's terminal
// to it.
package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"codeflower-app/internal/sandbox"
)

// maxFrame is the bridge's own limit (MAX_PAYLOAD in its server.js).
const maxFrame = 16 << 20

// rpcError is an error the bridge answered with.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// conn is a JSON-RPC connection to a bridge, which only uses its control
// methods: no ACP, so the bridge starts no chat for it.
type conn struct {
	ws      *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan message
	// Notifications, in order; closed when the connection ends.
	notifications chan message
	closed        chan struct{}
	err           error
}

// dial connects to the bridge behind a connection line, over TLS pinned to its
// fingerprint when it has one.
func dial(ctx context.Context, connection sandbox.Connection) (*conn, error) {
	address := connection.BridgeURL("/acp")
	address = "ws" + strings.TrimPrefix(address, "http")
	header := http.Header{}
	header.Set("Authorization", "Bearer "+connection.Token)
	ws, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: &http.Client{Transport: connection.Transport()},
	})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("the sandbox refused its token: it was restarted, or the line is old")
		}
		return nil, fmt.Errorf("could not reach the sandbox at %s:%d: %w", connection.Host, connection.Port, err)
	}
	ws.SetReadLimit(maxFrame)
	c := &conn{
		ws:            ws,
		pending:       map[string]chan message{},
		notifications: make(chan message, 256),
		closed:        make(chan struct{}),
	}
	go c.read()
	return c, nil
}

func (c *conn) read() {
	defer close(c.closed)
	defer close(c.notifications)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, waiting := range c.pending {
				close(waiting)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var received message
		if json.Unmarshal(data, &received) != nil {
			continue
		}
		if received.Method != "" {
			// Requests of the agent's are not for this connection: it opened no chat.
			if received.ID == nil {
				c.notifications <- received
			}
			continue
		}
		c.mu.Lock()
		waiting := c.pending[string(received.ID)]
		delete(c.pending, string(received.ID))
		c.mu.Unlock()
		if waiting != nil {
			waiting <- received
		}
	}
}

// call sends a request and decodes its answer into result (when not nil).
func (c *conn) call(ctx context.Context, method string, params any, result any) error {
	id := fmt.Sprintf("\"attach-%d\"", c.nextID.Add(1))
	waiting := make(chan message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return errors.New("the connection to the sandbox ended")
	}
	c.pending[id] = waiting
	c.mu.Unlock()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	select {
	case answer, ok := <-waiting:
		if !ok {
			return errors.New("the connection to the sandbox ended")
		}
		if answer.Error != nil {
			return answer.Error
		}
		if result != nil && answer.Result != nil {
			return json.Unmarshal(answer.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *conn) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "detached")
}
