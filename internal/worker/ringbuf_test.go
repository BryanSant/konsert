package worker

import (
	"bytes"
	"sync"
	"testing"
)

func TestRingBufferBasicAndWrap(t *testing.T) {
	rb := NewRingBuffer(10)
	if rb.Len() != 0 || rb.Snapshot() != nil {
		t.Fatalf("expected empty buffer")
	}

	_, _ = rb.Write([]byte("123456"))
	if got := string(rb.Snapshot()); got != "123456" {
		t.Fatalf("got %q want 123456", got)
	}

	// Exact fill
	_, _ = rb.Write([]byte("7890"))
	if got := string(rb.Snapshot()); got != "1234567890" {
		t.Fatalf("got %q want 1234567890", got)
	}
	if rb.Len() != 10 {
		t.Fatalf("got len %d want 10", rb.Len())
	}

	// Wrap around
	_, _ = rb.Write([]byte("abc"))
	if got := string(rb.Snapshot()); got != "4567890abc" {
		t.Fatalf("got %q want 4567890abc", got)
	}

	// Larger than capacity
	_, _ = rb.Write([]byte("0123456789XYZ"))
	if got := string(rb.Snapshot()); got != "3456789XYZ" {
		t.Fatalf("got %q want 3456789XYZ", got)
	}

	// Write crossing boundary after wrap
	_, _ = rb.Write([]byte("123456"))
	if got := string(rb.Snapshot()); got != "9XYZ123456" {
		t.Fatalf("got %q want 9XYZ123456", got)
	}
}

func TestRingBufferConcurrent(t *testing.T) {
	rb := NewRingBuffer(1024)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		chunk := bytes.Repeat([]byte("a"), 37)
		for range 500 {
			_, _ = rb.Write(chunk)
		}
	}()

	go func() {
		defer wg.Done()
		for range 500 {
			snap := rb.Snapshot()
			if len(snap) > 1024 {
				t.Errorf("snapshot exceeded cap: %d", len(snap))
			}
		}
	}()

	wg.Wait()
	if rb.Len() != 1024 {
		t.Fatalf("expected full buffer, got %d", rb.Len())
	}
}
