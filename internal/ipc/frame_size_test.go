package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
)

// A rejected frame must not even attempt a write to the daemon.
type frameWriteProbe struct {
	net.Conn
	writes int
}

func (c *frameWriteProbe) Write(p []byte) (int, error) {
	c.writes++
	return 0, fmt.Errorf("unexpected daemon write")
}

func TestClientFrameSizeBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra_%d", extra), func(t *testing.T) {
			// Predict the next ID's encoded width without consuming it.
			params := &RespondParams{Instructions: map[string]string{"R1": ""}}
			req := &Request{JSONRPC: "2.0", Method: MethodRespond, ID: reqID.Load() + 1}
			raw, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			req.Params = raw
			encoded, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			params.Instructions["R1"] = strings.Repeat("x", MaxFrameBytes-len(encoded)-1+extra)

			if extra != 0 {
				conn := &frameWriteProbe{}
				client := &Client{conn: conn, encoder: json.NewEncoder(conn)}
				err := client.Call(MethodRespond, params, nil)
				want := fmt.Sprintf("send %s request: encoded IPC frame is %d bytes, exceeds the %d-byte limit", MethodRespond, MaxFrameBytes+extra, MaxFrameBytes)
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v; want %s", err, want)
				}
				if conn.writes != 0 {
					t.Fatalf("attempted %d writes", conn.writes)
				}
				return
			}

			local, remote := net.Pipe()
			defer local.Close()
			server := NewServer()
			defer server.Close()
			server.Handle(MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var got RespondParams
				if err := json.Unmarshal(raw, &got); err != nil {
					return nil, err
				}
				if got.Instructions["R1"] != params.Instructions["R1"] {
					return nil, fmt.Errorf("guidance changed")
				}
				return &RespondResult{OK: true}, nil
			})
			done := make(chan struct{})
			go func() { defer close(done); server.handleConn(remote) }()
			scanner := bufio.NewScanner(local)
			scanner.Buffer(make([]byte, 0, MaxFrameBytes), MaxFrameBytes)
			client := &Client{conn: local, encoder: json.NewEncoder(local), scanner: scanner}
			var result RespondResult
			if err := client.Call(MethodRespond, params, &result); err != nil {
				t.Fatal(err)
			}
			if !result.OK {
				t.Fatal("at-limit request was not handled")
			}
			local.Close()
			<-done
		})
	}
}
