package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWaitIdleTracksWebSocketHandlers(t *testing.T) {
	srv, ts := newSecurityTestServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/client", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if err := srv.WaitIdle(short); err == nil {
		t.Fatal("WaitIdle must wait while a /ws/client handler is running")
	}

	srv.Hub().Close()
	idle, cancelIdle := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelIdle()
	if err := srv.WaitIdle(idle); err != nil {
		t.Fatalf("WaitIdle after Hub.Close: %v", err)
	}
}
