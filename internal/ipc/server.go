package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
)

// HandlerFunc processes a JSON-RPC request and returns a result or error.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (interface{}, error)

// StreamFunc owns a prepared streaming connection. send writes a JSON object;
// the function should block until streaming is complete and return when send
// reports a disconnected client.
type StreamFunc func(send func(interface{}) error) error

// StreamHandlerFunc prepares a stream before the server acknowledges the
// subscription. This closes the subscribe-then-reconcile race: callers cannot
// perform their first reconciliation until the handler has registered its
// event source. The returned StreamFunc runs after the acknowledgement;
// preparation resources must also be released when ctx is cancelled because
// an acknowledgement write can fail before StreamFunc starts.
type StreamHandlerFunc func(ctx context.Context, params json.RawMessage) (StreamFunc, error)

// Server listens on an IPC endpoint and dispatches JSON-RPC requests.
type Server struct {
	mu             sync.RWMutex
	handlers       map[string]HandlerFunc
	streamHandlers map[string]StreamHandlerFunc
	listener       net.Listener
	wg             sync.WaitGroup
	done           chan struct{}
	closeOnce      sync.Once
}

// NewServer creates a new IPC server.
func NewServer() *Server {
	return &Server{
		handlers:       make(map[string]HandlerFunc),
		streamHandlers: make(map[string]StreamHandlerFunc),
		done:           make(chan struct{}),
	}
}

// Handle registers a handler for a JSON-RPC method.
func (s *Server) Handle(method string, fn HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

// HandleStream registers a streaming handler for a JSON-RPC method.
// When this method is called, the server sends an initial OK response,
// then hands the connection to the handler for streaming. The connection
// closes when the handler returns.
func (s *Server) HandleStream(method string, fn StreamHandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamHandlers[method] = fn
}

// Listen binds the IPC endpoint without starting the accept loop. It lets the
// daemon measure bind separately, then start serving and prove real health
// before announcing readiness.
func (s *Server) Listen(socketPath string) error {
	ln, err := listen(socketPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.listener != nil {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("IPC server already listening")
	}
	s.listener = ln
	s.mu.Unlock()

	go func() {
		<-s.done
		_ = ln.Close()
	}()
	return nil
}

// Serve starts listening on the given IPC endpoint path. It blocks until
// Close is called, then returns nil.
func (s *Server) Serve(socketPath string) error {
	if err := s.Listen(socketPath); err != nil {
		return err
	}
	return s.ServeReady()
}

// ServeReady accepts requests from an endpoint already bound by Listen.
func (s *Server) ServeReady() error {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln == nil {
		return errors.New("IPC server is not listening")
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				s.wg.Wait()
				return nil
			default:
				if errors.Is(err, net.ErrClosed) {
					s.Close()
					s.wg.Wait()
					return nil
				}
				slog.Error("accept connection", "error", err)
				continue
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Close gracefully shuts down the server.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

// CloseListener closes the underlying listener without signaling server
// shutdown. This causes Accept to return net.ErrClosed, which the server
// detects and exits cleanly.
func (s *Server) CloseListener() {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln != nil {
		ln.Close()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	encoder := json.NewEncoder(conn)

	// Create a context that cancels when the server shuts down.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Warn("ipc request failed", "method", "<parse>", "error", "invalid json")
			resp := NewErrorResponse(0, ErrParseError, "invalid json")
			encoder.Encode(resp)
			continue
		}

		// Check for stream handler first.
		s.mu.RLock()
		streamHandler, isStream := s.streamHandlers[req.Method]
		s.mu.RUnlock()

		if isStream {
			stream, err := streamHandler(ctx, req.Params)
			if err != nil {
				slog.Warn("ipc stream request failed", "method", req.Method, "error", err)
				_ = encoder.Encode(NewErrorResponse(req.ID, ErrInternal, err.Error()))
				return
			}
			slog.Info("ipc stream request", "method", req.Method)
			// Acknowledge only after stream preparation has registered its event
			// source, so the client can safely reconcile as soon as this arrives.
			resp, _ := NewResponse(req.ID, map[string]bool{"ok": true})
			if err := encoder.Encode(resp); err != nil {
				slog.Error("write stream response", "error", err)
				return
			}
			send := func(event interface{}) error {
				return encoder.Encode(event)
			}
			if err := stream(send); err != nil {
				slog.Warn("ipc stream request failed", "method", req.Method, "error", err)
			}
			return // connection done after streaming
		}

		resp := s.dispatch(ctx, req)
		if err := encoder.Encode(resp); err != nil {
			slog.Error("write response", "error", err)
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) *Response {
	s.mu.RLock()
	handler, ok := s.handlers[req.Method]
	s.mu.RUnlock()

	if !ok {
		err := "method not found: " + req.Method
		slog.Warn("ipc request failed", "method", req.Method, "error", err)
		return NewErrorResponse(req.ID, ErrMethodNotFound, err)
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		slog.Warn("ipc request failed", "method", req.Method, "error", err)
		return NewErrorResponse(req.ID, ErrInternal, err.Error())
	}

	resp, err := NewResponse(req.ID, result)
	if err != nil {
		slog.Warn("ipc request failed", "method", req.Method, "error", err)
		return NewErrorResponse(req.ID, ErrInternal, "failed to marshal result")
	}
	if readOnlyMethod(req.Method) {
		slog.Debug("ipc request", "method", req.Method)
	} else {
		slog.Info("ipc request", "method", req.Method)
	}
	return resp
}

// readOnlyMethod is the single request-log policy for successful RPCs that
// only inspect daemon state. They can be called by health checks, dashboards,
// and recovery heartbeats without amplifying the lifecycle log. Failed reads
// still take the WARN path above.
func readOnlyMethod(method string) bool {
	switch method {
	case MethodHealth, MethodGetRun, MethodGetRuns, MethodGetRunsForHead, MethodGetActiveRun:
		return true
	default:
		return false
	}
}
