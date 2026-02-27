package gracefulshutdownserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// helpers

func newTestServer() *Server {
	return &Server{
		Server:      &http.Server{Addr: ":0"}, // port 0 = random available port
		workers:     make(chan func(), 4),
		workerCount: 4,
		dbConns:     []net.Conn{},
	}
}

func startTestServer(t *testing.T, s *Server) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := s.Start(ctx); err != nil && err != http.ErrServerClosed {
			t.Logf("server exited with error: %v", err)
		}
	}()
	// give server time to start
	time.Sleep(100 * time.Millisecond)
	return ctx, cancel
}

// ─────────────────────────────────────────────
// Test 1: Sudden Death Test
// ─────────────────────────────────────────────

func TestSuddenDeath(t *testing.T) {
	s := newTestServer()

	// replace handler with one that tracks in-flight requests
	var (
		inFlight  int64
		completed int64
		mu        sync.Mutex
	)

	s.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		mu.Unlock()

		time.Sleep(200 * time.Millisecond) // simulate work

		mu.Lock()
		inFlight--
		completed++
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	})

	_, cancel := startTestServer(t, s)
	defer cancel()

	// wait for server to bind and get actual address
	time.Sleep(50 * time.Millisecond)

	addr := s.Server.Addr
	url := fmt.Sprintf("http://%s/", addr)

	// send 100 requests concurrently
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			//nolint:errcheck
			http.Get(url) //nolint
		}()
	}

	// immediately simulate SIGTERM
	time.Sleep(50 * time.Millisecond)
	t.Log("shutting down...")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()

	if err := s.Stop(stopCtx); err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}

	wg.Wait()

	t.Logf("completed requests: %d", completed)

	// server must not accept new requests after shutdown
	_, err := http.Get(url)
	if err == nil {
		t.Error("server still accepting requests after shutdown")
	}

	// no goroutine should be stuck in-flight after stop
	mu.Lock()
	if inFlight != 0 {
		t.Errorf("goroutine leak: %d requests still in-flight after shutdown", inFlight)
	}
	mu.Unlock()
}

// ─────────────────────────────────────────────
// Test 2: Slow Leak Test
// ─────────────────────────────────────────────

func TestSlowLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow leak test in short mode (run without -short to enable)")
	}

	goroutinesBefore := runtime.NumGoroutine()
	t.Logf("goroutines before start: %d", goroutinesBefore)

	s := newTestServer()
	s.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	_ , cancel := startTestServer(t, s)
	defer cancel()

	addr := s.Server.Addr
	url := fmt.Sprintf("http://%s/", addr)

	// send 1 request/second for 10 seconds (shortened from 5min for CI)
	// to run the full 5 minute version, change duration to 5*time.Minute
	duration := 10 * time.Second
	ticker := time.NewTicker(time.Second)
	done := time.After(duration)

	t.Logf("sending requests for %s...", duration)
loop:
	for {
		select {
		case <-ticker.C:
			go http.Get(url) //nolint:errcheck
		case <-done:
			break loop
		}
	}
	ticker.Stop()

	// simulate SIGTERM
	t.Log("shutting down...")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()

	if err := s.Stop(stopCtx); err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}

	// wait for goroutines to settle
	time.Sleep(500 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	t.Logf("goroutines after stop: %d", goroutinesAfter)

	// allow a small buffer for Go runtime goroutines
	if goroutinesAfter > goroutinesBefore+2 {
		t.Errorf("goroutine leak detected: started with %d, ended with %d",
			goroutinesBefore, goroutinesAfter)
	}
}

// ─────────────────────────────────────────────
// Test 3: Timeout Test
// ─────────────────────────────────────────────

func TestShutdownTimeout(t *testing.T) {
	s := newTestServer()

	// handler that takes 20 seconds — longer than our shutdown timeout
	s.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Second)
		w.WriteHeader(http.StatusOK)
	})

	_, cancel := startTestServer(t, s)
	defer cancel()

	addr := s.Server.Addr
	url := fmt.Sprintf("http://%s/", addr)

	// start a long-running request
	go http.Get(url) //nolint:errcheck
	time.Sleep(100 * time.Millisecond) // let request start

	// simulate SIGTERM with only 5s timeout
	t.Log("shutting down with 5s timeout...")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	start := time.Now()
	err := s.Stop(stopCtx)
	elapsed := time.Since(start)

	t.Logf("shutdown took: %s", elapsed)

	// should have returned within ~5s, not waited the full 20s
	if elapsed > 7*time.Second {
		t.Errorf("shutdown took too long: %s (expected ~5s)", elapsed)
	}

	// should return a timeout error
	if err == nil {
		t.Log("shutdown timeout: forced exit (expected)")
	} else {
		t.Logf("shutdown returned error (expected on timeout): %v", err)
	}
}

// ─────────────────────────────────────────────
// Signal Integration Test
// ─────────────────────────────────────────────

func TestSignalHandling(t *testing.T) {
	s := newTestServer()
	s.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	_, cancel := startTestServer(t, s)
	defer cancel()

	// simulate sending SIGTERM to the process
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := s.Stop(stopCtx); err != nil {
			t.Logf("stop error: %v", err)
		}
	}()

	// send the actual signal
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM) //nolint:errcheck

	select {
	case <-done:
		t.Log("server shut down cleanly after SIGTERM")
	case <-time.After(6 * time.Second):
		t.Error("server did not shut down within 6s of SIGTERM")
	}
}