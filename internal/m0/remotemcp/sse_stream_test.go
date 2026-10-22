package remotemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestA26SSECompletesBeforeEOF reproduces Acceptance report F3: a complete,
// matching JSON-RPC SSE event followed by an open stream must complete the
// request promptly instead of waiting for EOF/timeout.
func TestA26SSECompletesBeforeEOF(t *testing.T) {
	held := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w,
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"serverInfo\":{\"name\":\"sse-hold\",\"version\":\"1\"}}}\n\n",
			string(req.ID))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done() // keep the SSE stream open after the complete event
		close(held)
	}))
	defer srv.Close()

	client, err := New(Config{URL: srv.URL, Timeout: 2 * time.Second, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	start := time.Now()
	info, err := client.Initialize(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Initialize over held SSE stream: %v", err)
	}
	if info.ProtocolVersion == "" {
		t.Fatal("empty protocol version")
	}
	if elapsed > time.Second {
		t.Fatalf("matching SSE event waited for EOF/timeout: %s", elapsed)
	}
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("server stream was not closed after the client consumed the event")
	}
}

// TestA26SSEErrorCompletesBeforeEOF verifies a matching JSON-RPC error event is
// surfaced promptly even if the server keeps the stream open.
func TestA26SSEErrorCompletesBeforeEOF(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w,
			"data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32603,\"message\":\"synthetic SSE error\"}}\n\n",
			string(req.ID))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := New(Config{URL: srv.URL, Timeout: 2 * time.Second, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := time.Now()
	_, err = client.Initialize(context.Background())
	if !errors.Is(err, ErrRPC) {
		t.Fatalf("Initialize error = %v, want ErrRPC", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("matching SSE error waited for EOF/timeout: %s", elapsed)
	}
}

// TestA26SSENoMatchIsBounded verifies a held stream without a matching id is
// still bounded by the request deadline, not by EOF.
func TestA26SSENoMatchIsBounded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := New(Config{URL: srv.URL, Timeout: 200 * time.Millisecond, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := time.Now()
	_, err = client.Initialize(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Initialize error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("held stream was not bounded: %s", elapsed)
	}
}

// TestA26SSEIncompleteEventIsBounded verifies a malformed/truncated event is
// rejected promptly without waiting for stream EOF/timeout.
func TestA26SSEIncompleteEventIsBounded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":\n\n", string(req.ID))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := New(Config{URL: srv.URL, Timeout: 2 * time.Second, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := time.Now()
	_, err = client.Initialize(context.Background())
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Initialize error = %v, want ErrProtocol", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("malformed SSE event waited for timeout: %s", elapsed)
	}
}

// TestA26SSEOversizedEventIsBounded verifies the streaming parser enforces the
// response byte bound instead of buffering an unbounded event.
func TestA26SSEOversizedEventIsBounded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("a", maxResponseBytes+1))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := New(Config{URL: srv.URL, Timeout: 2 * time.Second, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Initialize(context.Background())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Initialize error = %v, want ErrTooLarge", err)
	}
}

// TestA26SSECancellationIsBounded verifies caller cancellation unblocks a held
// stream without a matching event.
func TestA26SSECancellationIsBounded(t *testing.T) {
	sent := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(sent)
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, err := New(Config{URL: srv.URL, Timeout: 5 * time.Second, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(ctx)
		done <- err
	}()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("Initialize error = %v, want ErrCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock held SSE stream")
	}
}
