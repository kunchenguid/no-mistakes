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

// StreamHandlerFunc takes over a connection for streaming.
// send writes a JSON object to the connection. The function should block
// until streaming is complete. When send returns an error (client disconnected),
// the handler should return.
type StreamHandlerFunc func(ctx context.Context, params json.RawMessage, send func(interface{}) error) error

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

// Serve starts listening on the given IPC endpoint path.
// It blocks until Close is called, then returns nil.
func (s *Server) Serve(socketPath string) error {
	ln, err := listen(socketPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	go func() {
		<-s.done
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				s.wg.Wait()
				return nil
			default:
				if errors.Is(err, net.ErrClosed) {
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
			resp := NewErrorResponse(0, ErrParseError, "invalid json")
			encoder.Encode(resp)
			continue
		}

		// Check for stream handler first.
		s.mu.RLock()
		streamHandler, isStream := s.streamHandlers[req.Method]
		s.mu.RUnlock()

		if isStream {
			// Send initial OK response.
			resp, _ := NewResponse(req.ID, map[string]bool{"ok": true})
			if err := encoder.Encode(resp); err != nil {
				slog.Error("write stream response", "error", err)
				return
			}
			// Hand connection to stream handler. It blocks until done.
			send := func(event interface{}) error {
				return encoder.Encode(event)
			}
			if err := streamHandler(ctx, req.Params, send); err != nil {
				slog.Debug("stream handler ended", "method", req.Method, "error", err)
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
		return NewErrorResponse(req.ID, ErrMethodNotFound, "method not found: "+req.Method)
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		return NewErrorResponse(req.ID, ErrInternal, err.Error())
	}

	resp, err := NewResponse(req.ID, result)
	if err != nil {
		return NewErrorResponse(req.ID, ErrInternal, "failed to marshal result")
	}
	return resp
}
