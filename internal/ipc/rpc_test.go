package ipc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestServerClientRoundTrip_EnableWindowsCI(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type echoParams struct {
		Message string `json:"message"`
	}
	type echoResult struct {
		Echo string `json:"echo"`
	}

	srv.Handle("echo", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p echoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return echoResult{Echo: p.Message}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result echoResult
	if err := c.Call("echo", echoParams{Message: "hello"}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Echo != "hello" {
		t.Errorf("got echo=%q, want %q", result.Echo, "hello")
	}
}

func TestMethodNotFound(t *testing.T) {
	sock := socketPath(t)
	startServer(t, sock)

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result json.RawMessage
	err = c.Call("nonexistent", nil, &result)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	rpcErr, ok := err.(*ipc.RPCError)
	if !ok {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != ipc.ErrMethodNotFound {
		t.Errorf("got code %d, want %d", rpcErr.Code, ipc.ErrMethodNotFound)
	}
}

func TestHandlerError(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("fail", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("something broke")
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result json.RawMessage
	err = c.Call("fail", nil, &result)
	if err == nil {
		t.Fatal("expected error")
	}
	rpcErr, ok := err.(*ipc.RPCError)
	if !ok {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != ipc.ErrInternal {
		t.Errorf("got code %d, want %d", rpcErr.Code, ipc.ErrInternal)
	}
	if rpcErr.Message != "something broke" {
		t.Errorf("got message=%q, want %q", rpcErr.Message, "something broke")
	}
}

func TestMultipleClients_EnableWindowsCI(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type addParams struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	type addResult struct {
		Sum int `json:"sum"`
	}

	srv.Handle("add", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p addParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return addResult{Sum: p.A + p.B}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, err := ipc.Dial(sock)
			if err != nil {
				t.Errorf("client %d dial: %v", n, err)
				return
			}
			defer c.Close()

			var result addResult
			if err := c.Call("add", addParams{A: n, B: 10}, &result); err != nil {
				t.Errorf("client %d call: %v", n, err)
				return
			}
			if result.Sum != n+10 {
				t.Errorf("client %d: got sum=%d, want %d", n, result.Sum, n+10)
			}
		}(i)
	}
	wg.Wait()
}

func TestMultipleCallsOnSameConnection(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type echoParams struct {
		Message string `json:"message"`
	}
	type echoResult struct {
		Echo string `json:"echo"`
	}

	srv.Handle("echo", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p echoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return echoResult{Echo: p.Message}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("msg-%d", i)
		var result echoResult
		if err := c.Call("echo", echoParams{Message: msg}, &result); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if result.Echo != msg {
			t.Errorf("call %d: got echo=%q, want %q", i, result.Echo, msg)
		}
	}
}

func TestNilParams(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("health", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok"}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result ipc.HealthResult
	if err := c.Call("health", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("got status=%q, want %q", result.Status, "ok")
	}
}

func TestHealthRequestsDoNotLogAtInfo(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle("fail", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("something broke")
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var health ipc.HealthResult
	if err := c.Call(ipc.MethodHealth, nil, &health); err != nil {
		t.Fatalf("health call: %v", err)
	}

	var raw json.RawMessage
	err = c.Call("fail", nil, &raw)
	if err == nil {
		t.Fatal("expected fail call error")
	}

	logOutput := logs.String()
	if strings.Contains(logOutput, "method=health") {
		t.Fatalf("health request should not log at info: %s", logOutput)
	}
	if !strings.Contains(logOutput, "msg=\"ipc request failed\" method=fail") {
		t.Fatalf("failed request log missing: %s", logOutput)
	}
}

func TestCallWithNilResult(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("noop", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return map[string]bool{"ok": true}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Call with nil result pointer — should succeed without unmarshaling result.
	if err := c.Call("noop", nil, nil); err != nil {
		t.Fatalf("call with nil result: %v", err)
	}
}
