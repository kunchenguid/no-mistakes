package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var dialNetworkWithTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).Dial(network, address)
}

// ConnectTimeoutError reports a daemon IPC connect attempt that exceeded the
// bounded client timeout.
type ConnectTimeoutError struct {
	SocketPath      string
	TimeoutDuration time.Duration
	Err             error
}

func (e *ConnectTimeoutError) Error() string {
	return fmt.Sprintf("daemon socket %s did not accept a connection within %s", e.SocketPath, e.TimeoutDuration)
}

func (e *ConnectTimeoutError) Unwrap() error { return e.Err }

func (e *ConnectTimeoutError) Timeout() bool { return true }

// IsConnectTimeout reports whether err was caused by a bounded IPC connect
// timeout.
func IsConnectTimeout(err error) bool {
	var timeoutErr *ConnectTimeoutError
	return errors.As(err, &timeoutErr)
}

func connectTimeout() time.Duration {
	value := os.Getenv("NM_DAEMON_CONNECT_TIMEOUT")
	if value == "" {
		p, err := paths.New()
		if err != nil {
			return config.DefaultDaemonConnectTimeout
		}
		cfg, err := config.LoadGlobal(p.ConfigFile())
		if err != nil {
			return config.DefaultDaemonConnectTimeout
		}
		return cfg.DaemonConnectTimeout
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return config.DefaultDaemonConnectTimeout
	}
	return d
}

// Client connects to the IPC server over the platform transport.
type Client struct {
	conn    net.Conn
	encoder *json.Encoder
	scanner *bufio.Scanner
	mu      sync.Mutex // serializes calls on a single connection
}

const (
	// DefaultDialTimeout is the read deadline used for the daemon health check
	// dial made by callers outside this package (see internal/daemon/selfexec.go).
	DefaultDialTimeout = 250 * time.Millisecond
	defaultCallTimeout = 30 * time.Second
)

// Dial connects to the IPC server at the given endpoint path.
func Dial(socketPath string) (*Client, error) {
	conn, err := dialEndpoint(socketPath)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	return &Client{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		scanner: scanner,
	}, nil
}

func dialEndpoint(socketPath string) (net.Conn, error) {
	timeout := connectTimeout()
	conn, err := dial(socketPath, timeout)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("dial ipc: %w", &ConnectTimeoutError{
				SocketPath:      socketPath,
				TimeoutDuration: timeout,
				Err:             err,
			})
		}
		return nil, fmt.Errorf("dial ipc: %w", err)
	}
	return conn, nil
}

// Call sends a JSON-RPC request and waits for the response.
// The result is unmarshaled into the provided pointer.
// If the server returns a JSON-RPC error, it is returned as *RPCError.
func (c *Client) Call(method string, params interface{}, result interface{}) error {
	return c.CallWithTimeout(method, params, result, defaultCallTimeout)
}

// CallWithTimeout is Call with a caller-selected read deadline.
func (c *Client) CallWithTimeout(method string, params interface{}, result interface{}, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req, err := NewRequest(method, params)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if err := c.encoder.Encode(req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	c.conn.SetReadDeadline(time.Now().Add(timeout))
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
	conn, err := dialEndpoint(socketPath)
	if err != nil {
		return nil, nil, err
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
