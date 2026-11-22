package worker

import "sync"

// DefaultRingBufferSize is 512 KB per PTY session.
const DefaultRingBufferSize = 512 * 1024

// RingBuffer is a thread-safe fixed-capacity circular byte buffer.
// When full, newly written bytes overwrite the oldest bytes.
type RingBuffer struct {
	mu    sync.Mutex
	buf   []byte
	cap   int
	write int // next write index
	full  bool
}

// NewRingBuffer creates a RingBuffer with the given byte capacity.
// If capacity <= 0, DefaultRingBufferSize is used.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = DefaultRingBufferSize
	}
	return &RingBuffer{
		buf: make([]byte, capacity),
		cap: capacity,
	}
}

// Write appends p to the circular buffer, overwriting oldest bytes if necessary.
func (r *RingBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if n >= r.cap {
		// Keep only the trailing r.cap bytes.
		copy(r.buf, p[n-r.cap:])
		r.write = 0
		r.full = true
		return n, nil
	}

	first := r.cap - r.write
	if n <= first {
		copy(r.buf[r.write:], p)
		r.write += n
		if r.write == r.cap {
			r.write = 0
			r.full = true
		}
	} else {
		copy(r.buf[r.write:], p[:first])
		rem := n - first
		copy(r.buf[0:], p[first:])
		r.write = rem
		r.full = true
	}
	return n, nil
}

// Snapshot returns a contiguous chronological copy of all buffered bytes.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		if r.write == 0 {
			return nil
		}
		out := make([]byte, r.write)
		copy(out, r.buf[:r.write])
		return out
	}

	out := make([]byte, r.cap)
	tailLen := r.cap - r.write
	copy(out, r.buf[r.write:])
	copy(out[tailLen:], r.buf[:r.write])
	return out
}

// Len returns the number of valid bytes currently stored.
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.cap
	}
	return r.write
}

// Cap returns the fixed capacity of the ring buffer.
func (r *RingBuffer) Cap() int {
	return r.cap
}

// Reset clears the buffer.
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write = 0
	r.full = false
}
