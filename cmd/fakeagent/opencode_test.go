package main

import "testing"

func TestFakeOpencodeServerUnsubscribeLeavesCopiedSubscriberSafe(t *testing.T) {
	srv := newFakeOpencodeServer(defaultScenario())
	ch := make(chan []byte, 1)
	srv.subscribe(ch)

	srv.mu.Lock()
	subs := append([]chan []byte(nil), srv.subscribers...)
	srv.mu.Unlock()

	srv.unsubscribe(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("send to copied subscriber panicked after unsubscribe: %v", r)
		}
	}()

	subs[0] <- []byte("data: {}\n\n")
}
