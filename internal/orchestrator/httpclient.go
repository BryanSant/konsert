package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Transport and retry defaults for provider HTTP calls. There is deliberately no
// overall http.Client.Timeout: a healthy SSE stream may legitimately run for
// minutes. Instead every phase that can stall is bounded: dialing, TLS, waiting
// for response headers, and gaps between streamed bytes (idle watchdog). The
// caller's context bounds the whole turn.
const (
	defaultDialTimeout           = 15 * time.Second
	defaultTLSHandshakeTimeout   = 15 * time.Second
	defaultResponseHeaderTimeout = 3 * time.Minute // non-streaming calls and cold model loads (Ollama) send headers late
	defaultIdleConnTimeout       = 90 * time.Second
	defaultStreamIdleTimeout     = 2 * time.Minute
)

// newHTTPClient returns an http.Client whose transport bounds every stall-prone phase.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	tr.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	tr.IdleConnTimeout = defaultIdleConnTimeout
	tr.ExpectContinueTimeout = time.Second
	return &http.Client{Transport: tr}
}

// retryPolicy controls retries of 429/5xx responses and connection failures.
// Retries only happen before any response body is consumed, so a retried call
// never produces duplicate streamed deltas.
type retryPolicy struct {
	MaxAttempts   int           // total attempts including the first; <=1 disables retries
	BaseDelay     time.Duration // first backoff delay, doubled per attempt, with jitter
	MaxDelay      time.Duration // cap on computed backoff
	MaxRetryAfter time.Duration // a server-requested Retry-After longer than this is not waited out
}

var defaultRetryPolicy = retryPolicy{
	MaxAttempts:   4,
	BaseDelay:     500 * time.Millisecond,
	MaxDelay:      20 * time.Second,
	MaxRetryAfter: 60 * time.Second,
}

// httpOpts is embedded by each provider.
type httpOpts struct {
	client      *http.Client
	retry       retryPolicy
	idleTimeout time.Duration // max gap between bytes of a response body; 0 disables
}

func defaultHTTPOpts() httpOpts {
	return httpOpts{client: newHTTPClient(), retry: defaultRetryPolicy, idleTimeout: defaultStreamIdleTimeout}
}

// ErrStreamIdle is returned when a provider stops sending bytes mid-response.
var ErrStreamIdle = errors.New("LLM response stalled: no data received within the idle timeout")

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529: // Anthropic "overloaded"
		return true
	}
	return false
}

// isRetryableNetErr reports connection-establishment failures. Errors after a
// request may have reached the server (e.g. header timeouts) are not retried,
// to avoid paying twice for a generation that is already running.
func isRetryableNetErr(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// parseRetryAfter accepts delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay << attempt
	if d <= 0 || d > p.MaxDelay {
		d = p.MaxDelay
	}
	// Full jitter in [d/2, d].
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// postJSON sends body to url, retrying per policy, and returns the final
// response. Non-2xx responses are returned (not converted to errors) so callers
// can format provider-specific messages. The response body is wrapped with the
// idle watchdog; closing it releases all resources.
func (o httpOpts) postJSON(ctx context.Context, url string, header http.Header, body []byte) (*http.Response, error) {
	client := o.client
	if client == nil {
		client = newHTTPClient()
	}
	attempts := o.retry.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reqCtx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := client.Do(req)
		last := attempt+1 >= attempts
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if last || !isRetryableNetErr(err) {
				return nil, err
			}
			if sErr := sleepCtx(ctx, o.retry.backoff(attempt)); sErr != nil {
				return nil, sErr
			}
			continue
		}
		if !last && isRetryableStatus(resp.StatusCode) {
			wait := o.retry.backoff(attempt)
			ra, hasRA := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			if hasRA {
				if o.retry.MaxRetryAfter > 0 && ra > o.retry.MaxRetryAfter {
					// Server asked for a longer pause than we are willing to hold the turn for.
					resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
					return resp, nil
				}
				wait = ra
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			cancel()
			if sErr := sleepCtx(ctx, wait); sErr != nil {
				return nil, sErr
			}
			continue
		}
		resp.Body = newIdleReader(resp.Body, o.idleTimeout, cancel)
		return resp, nil
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// idleReader cancels the request if no bytes arrive for the idle timeout, which
// unblocks a hung SSE stream instead of leaking the goroutine forever.
type idleReader struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
	cancel  context.CancelFunc
}

func newIdleReader(rc io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) io.ReadCloser {
	r := &idleReader{rc: rc, timeout: timeout, cancel: cancel}
	if timeout > 0 {
		r.timer = time.AfterFunc(timeout, func() {
			r.fired.Store(true)
			cancel()
		})
	}
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if r.timer != nil && n > 0 {
		r.timer.Reset(r.timeout)
	}
	if err != nil && err != io.EOF && r.fired.Load() {
		return n, fmt.Errorf("%w (%s)", ErrStreamIdle, r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	err := r.rc.Close()
	r.cancel()
	return err
}
