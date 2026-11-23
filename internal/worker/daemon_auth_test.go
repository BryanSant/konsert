package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

func unauthorizedHub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"invalid fleet token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestDaemonExitsClearlyWithoutFleetToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, hits := unauthorizedHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker",
		WorkerID:      "auth-test", Name: "auth-test", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.Run(ctx)
	if !errors.Is(err, ErrFleetTokenRequired) {
		t.Fatalf("expected ErrFleetTokenRequired, got %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected a single attempt without a token, got %d", n)
	}
}

func TestDaemonBacksOffSlowlyOnRejectedToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, hits := unauthorizedHub(t)
	d := NewDaemon(config.WorkerConfig{
		ControllerURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker",
		FleetToken:    "wrong", WorkerID: "auth-test-2", Name: "auth-test-2", Tags: map[string]string{},
	}, drivers.DefaultRegistry, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = d.Run(ctx)
	if n := hits.Load(); n != 1 {
		t.Fatalf("a rejected token must wait the maximum backoff before retrying; got %d attempts in 3s", n)
	}
}
