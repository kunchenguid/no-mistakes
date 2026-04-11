package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client connects to the IPC server over the platform transport.
type Client struct {
	conn    net.Conn
	encoder *json.Encoder
	scanner *bufio.Scanner
	mu      sync.Mutex // serializes calls on a single connection
}

// Dial connects to the IPC server at the given endpoint path.
func Dial(socketPath string) (*Client, error) {
	conn, err := dial(socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial ipc: %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	return &Client{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		scanner: scanner,
	}, nil
}

// Call sends a JSON-RPC request and waits for the response.
// The result is unmarshaled into the provided pointer.
// If the server returns a JSON-RPC error, it is returned as *RPCError.
func (c *Client) Call(method string, params interface{}, result interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req, err := NewRequest(method, params)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if err := c.encoder.Encode(req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		return fmt.Errorf("read response: connection closed")
	}

	var resp Response
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if resp.Error != nil {
		return resp.Error
	}

	if result != nil && resp.Result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}

	return nil
}

// Close disconnects from the server.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Subscribe opens a dedicated connection and subscribes to events for a run.
// Returns an event channel, a cancel function (to stop and clean up), and an error.
// The channel is closed when the run completes, the connection drops, or cancel is called.
func Subscribe(socketPath string, params *SubscribeParams) (<-chan Event, func(), error) {
	conn, err := dial(socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("dial ipc: %w", err)
	}
	encoder := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Send subscribe request.
	req, err := NewRequest(MethodSubscribe, params)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := encoder.Encode(req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	// Read initial response.
	if !scanner.Scan() {
		conn.Close()
		if err := scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("read response: %w", err)
		}
		return nil, nil, fmt.Errorf("read response: connection closed")
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("parse response: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, nil, resp.Error
	}

	// Stream events.
	ch := make(chan Event, 64)
	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			conn.Close()
		})
	}

	go func() {
		defer close(ch)
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				continue // skip malformed events
			}
			select {
			case ch <- event:
			case <-done:
				return
			}
		}
	}()

	return ch, cancel, nil
}
