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

func TestServerClientRoundTrip(t *testing.T) {
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

func TestMultipleClients(t *testing.T) {
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

func TestSuccessfulReadRequestsDoNotLogAtInfo(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	readMethods := []string{
		ipc.MethodHealth,
		ipc.MethodGetRun,
		ipc.MethodGetRuns,
		ipc.MethodGetRunsForHead,
		ipc.MethodGetActiveRun,
	}
	for _, method := range readMethods {
		srv.Handle(method, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
			return map[string]bool{"ok": true}, nil
		})
	}

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
	for _, method := range readMethods {
		var raw json.RawMessage
		if err := c.Call(method, nil, &raw); err != nil {
			t.Fatalf("%s call: %v", method, err)
		}
	}

	if logOutput := logs.String(); logOutput != "" {
		t.Fatalf("successful read requests wrote %d INFO bytes, want 0:\n%s", len(logOutput), logOutput)
	}
}

func TestSuccessfulReadRequestsLogAtDebug(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{}, nil
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	var result ipc.GetRunResult
	if err := c.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: "run-1"}, &result); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, `level=DEBUG msg="ipc request" method=get_run`) {
		t.Fatalf("successful read missing DEBUG record: %s", got)
	}
}

func TestRequestLoggingKeepsMutationsAndFailuresVisible(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)
	srv.Handle(ipc.MethodRerun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.RerunResult{RunID: "run-1"}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("database unavailable")
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
	var rerun ipc.RerunResult
	if err := c.Call(ipc.MethodRerun, nil, &rerun); err != nil {
		t.Fatalf("rerun call: %v", err)
	}
	var raw json.RawMessage
	if err := c.Call(ipc.MethodGetRun, nil, &raw); err == nil {
		t.Fatal("expected failed read call error")
	}
	if err := c.Call("unknown_method", nil, &raw); err == nil {
		t.Fatal("expected unknown method error")
	}

	logOutput := logs.String()
	for _, want := range []string{
		"level=INFO msg=\"ipc request\" method=rerun",
		"level=WARN msg=\"ipc request failed\" method=get_run error=\"database unavailable\"",
		"level=WARN msg=\"ipc request failed\" method=unknown_method",
	} {
		if !strings.Contains(logOutput, want) {
			t.Errorf("request log missing %q:\n%s", want, logOutput)
		}
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
