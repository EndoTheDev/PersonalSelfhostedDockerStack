package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestTransport builds a retryTransport against a real Server (via
// NewServer) pointed at the test upstream, with two plain keys.
func newTestTransport(t *testing.T, upstream string) (*retryTransport, *Server) {
	t.Helper()
	cfg := Config{
		Upstream:  upstream,
		Cooldown:  Duration(60 * time.Second),
		Port:      "0",
		Timeout:   Duration(10 * time.Second),
		MasterKey: mustMasterKey(t),
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	// NewServer with a nil store builds an empty pool; swap in two keys so
	// Acquire succeeds and the retry path has a second key to draw.
	srv.poolMu.Lock()
	srv.pool = &keyPool{keys: []*keyState{{key: "k1"}, {key: "k2"}}}
	srv.poolMu.Unlock()
	rt := newRetryTransport(func(string, ...any) {}, srv.st, srv)
	return rt, srv
}

// The upstream accepts the first request, then resets the connection
// (simulating ollama's edge reaping a stale connection), and answers every
// later request normally.
func resetThenAnswer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, conn)
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetLinger(0) // RST on close
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	return srv, &hits
}

// RoundTrip must transparently retry once on a connection-reset network
// error and succeed on the fresh connection.
func TestRoundTripRetriesOnConnectionReset(t *testing.T) {
	up, hits := resetThenAnswer(t)
	defer up.Close()

	rt, srv := newTestTransport(t, up.URL)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL, strings.NewReader(`{"model":"test"}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed after retry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("expected upstream hit twice (reset + retry), got %d", got)
	}
	if n := srv.st.upstreamErr.Load(); n != 1 {
		t.Fatalf("expected 1 recorded upstream error, got %d", n)
	}
	if msg, _ := srv.st.upstreamErrInfo(); msg == "" {
		t.Fatal("expected last upstream error text recorded")
	}
}

// A hard-failing upstream must produce a clean error and record counts.
func TestRoundTripFailsCleanlyWhenUpstreamDead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			c.Close()
		}
	}()

	rt, srv := newTestTransport(t, "http://"+ln.Addr().String())

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+ln.Addr().String(), strings.NewReader(`{}`))
	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected an error when upstream is dead")
	}
	if n := srv.st.upstreamErr.Load(); n == 0 {
		t.Fatal("expected upstream errors recorded")
	}
}

// Client cancellations must count as client_cancels, NOT upstream_errors.
func TestContextCanceledCountsAsClientCancel(t *testing.T) {
	st := newStats(16)
	st.recordUpstreamError(context.Canceled)
	if n := st.upstreamErr.Load(); n != 0 {
		t.Fatalf("context.Canceled must not count as upstream error, got %d", n)
	}
	if n := st.clientCancels.Load(); n != 1 {
		t.Fatalf("expected 1 client cancel, got %d", n)
	}
	// A real network error must land in the upstream bucket.
	st.recordUpstreamError(io.ErrUnexpectedEOF)
	if n := st.upstreamErr.Load(); n != 1 {
		t.Fatalf("EOF must count as upstream error, got %d", n)
	}
	if n := st.clientCancels.Load(); n != 1 {
		t.Fatalf("EOF must not change client cancels, got %d", n)
	}
	if msg, _ := st.upstreamErrInfo(); msg == "" {
		t.Fatal("expected upstream error text recorded")
	}
}

// Snapshot must carry the new upstream-error fields.
func TestSnapshotCarriesUpstreamErrorFields(t *testing.T) {
	up, _ := resetThenAnswer(t)
	defer up.Close()

	rt, srv := newTestTransport(t, up.URL)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL, strings.NewReader(`{"model":"t"}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	snap := srv.Snapshot()
	if snap.Stats.UpstreamErrors != 1 {
		t.Fatalf("snapshot upstream_errors = %d, want 1", snap.Stats.UpstreamErrors)
	}
	if snap.Stats.LastUpstreamError == "" || snap.Stats.LastUpstreamErrorAt == 0 {
		t.Fatal("snapshot must carry last upstream error text + time")
	}
}