package remotemcp_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/remotemcp"
)

const syntheticToken = "synthetic-bearer-token-not-a-real-secret"

type recordedRequest struct {
	method          string
	authorization   string
	accept          string
	contentType     string
	sessionID       string
	protocolVersion string
	body            []byte
}

// mcpServer is a synthetic remote MCP endpoint. It never contacts Home
// Assistant; every tool is fabricated test data.
type mcpServer struct {
	mu           sync.Mutex
	requests     []recordedRequest
	calledTools  []string
	tools        []map[string]any
	requireToken string
	statusByMeth map[string]int
	sleep        time.Duration
	sse          bool
	rpcErrorCall bool
	isErrorCall  bool
	hugeInit     bool
	callText     string
	sessionID    string
	noSessionHdr bool
}

func newMCPServer(t *testing.T) *mcpServer {
	t.Helper()
	return &mcpServer{
		requireToken: syntheticToken,
		sessionID:    "sess-remote-1",
		callText:     "synthetic read result",
		statusByMeth: map[string]int{},
		tools: []map[string]any{
			{
				"name":        "demo.read_status",
				"description": "synthetic read-only status",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"entity": map[string]any{"type": "string"}},
				},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			{
				"name":        "demo.set_volume",
				"description": "synthetic write tool that must never be called",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"volume": map[string]any{"type": "integer"}},
				},
				"annotations": map[string]any{"readOnlyHint": false},
			},
			{
				"name":        "demo.no_annotation",
				"description": "synthetic tool without read-only annotation",
				"inputSchema": map[string]any{"type": "object"},
			},
		},
	}
}

func (s *mcpServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			method:          req.Method,
			authorization:   r.Header.Get("Authorization"),
			accept:          r.Header.Get("Accept"),
			contentType:     r.Header.Get("Content-Type"),
			sessionID:       r.Header.Get("Mcp-Session-Id"),
			protocolVersion: r.Header.Get("MCP-Protocol-Version"),
			body:            body,
		})
		s.mu.Unlock()

		if s.requireToken != "" && r.Header.Get("Authorization") != "Bearer "+s.requireToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="synthetic"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status, ok := s.statusByMeth[req.Method]; ok {
			w.WriteHeader(status)
			return
		}
		if s.sleep > 0 {
			select {
			case <-time.After(s.sleep):
			case <-r.Context().Done():
				return
			}
		}

		switch req.Method {
		case "initialize":
			if s.hugeInit {
				w.Header().Set("Content-Type", "application/json")
				payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","pad":"%s"}}`,
					string(req.ID), strings.Repeat("x", limits.MaxJSONBytes))
				_, _ = w.Write([]byte(payload))
				return
			}
			result := map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "synthetic-ha", "version": "0.1.0"},
			}
			if s.sse {
				s.writeSSE(w, req.ID, result)
				return
			}
			if !s.noSessionHdr {
				w.Header().Set("Mcp-Session-Id", s.sessionID)
			}
			s.writeResult(w, req.ID, result)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			result := map[string]any{"tools": s.tools}
			if s.sse {
				s.writeSSE(w, req.ID, result)
				return
			}
			s.writeResult(w, req.ID, result)
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			s.mu.Lock()
			s.calledTools = append(s.calledTools, params.Name)
			s.mu.Unlock()
			if s.rpcErrorCall {
				s.writeRPCError(w, req.ID, -32601, "synthetic method not found")
				return
			}
			if s.isErrorCall {
				s.writeResult(w, req.ID, map[string]any{
					"isError": true,
					"content": []any{map[string]any{"type": "text", "text": "synthetic tool failure"}},
				})
				return
			}
			result := map[string]any{
				"content": []any{map[string]any{"type": "text", "text": s.callText}},
			}
			if s.sse {
				s.writeSSE(w, req.ID, result)
				return
			}
			s.writeResult(w, req.ID, result)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *mcpServer) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	_, _ = w.Write(payload)
}

func (s *mcpServer) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	_, _ = w.Write(payload)
}

func (s *mcpServer) writeSSE(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n")
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
}

func (s *mcpServer) snapshot() ([]recordedRequest, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...), append([]string(nil), s.calledTools...)
}

func newTestClient(t *testing.T, server *mcpServer) (*remotemcp.Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(server.handler())
	t.Cleanup(ts.Close)
	client, err := remotemcp.New(remotemcp.Config{
		URL:         ts.URL,
		BearerToken: server.requireToken,
		Timeout:     2 * time.Second,
		HTTPClient:  ts.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, ts
}

// ---------------------------------------------------------------------------
// A26: Streamable HTTP initialize/list/read-only tool call.
// ---------------------------------------------------------------------------

func TestA26StreamableHTTPInitializeListAndReadOnlyCall(t *testing.T) {
	server := newMCPServer(t)
	client, _ := newTestClient(t, server)
	ctx := context.Background()

	info, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "synthetic-ha" || info.Version != "0.1.0" {
		t.Fatalf("server info = %+v", info)
	}
	if info.ProtocolVersion != "2025-03-26" {
		t.Fatalf("protocol version = %q", info.ProtocolVersion)
	}
	if client.SessionID() != "sess-remote-1" {
		t.Fatalf("session id = %q", client.SessionID())
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("discovered %d tools, want 3", len(tools))
	}
	if tools[0].Name != "demo.read_status" || !strings.Contains(string(tools[0].Annotations), "readOnlyHint") {
		t.Fatalf("read-only tool missing annotation: %+v", tools[0])
	}
	if !strings.Contains(string(tools[0].InputSchema), "entity") {
		t.Fatalf("input schema not preserved: %s", tools[0].InputSchema)
	}

	res, err := client.CallReadOnlyTool(ctx, "demo.read_status", json.RawMessage(`{"entity":"synthetic.light"}`))
	if err != nil {
		t.Fatalf("CallReadOnlyTool: %v", err)
	}
	if res.Text() != "synthetic read result" || res.IsError {
		t.Fatalf("call result = %+v", res)
	}

	requests, called := server.snapshot()
	if len(requests) != 4 {
		t.Fatalf("server saw %d requests, want initialize + notification + tools/list + tools/call", len(requests))
	}
	if len(called) != 1 || called[0] != "demo.read_status" {
		t.Fatalf("server executed tools %v, want only demo.read_status", called)
	}
	for _, name := range called {
		if name == "demo.set_volume" {
			t.Fatal("remote MCP client called a write tool")
		}
	}

	initReq := requests[0]
	if initReq.method != "initialize" || initReq.authorization != "Bearer "+syntheticToken {
		t.Fatalf("initialize request headers = %+v", initReq)
	}
	if initReq.contentType != "application/json" {
		t.Fatalf("Content-Type = %q", initReq.contentType)
	}
	for _, want := range []string{"application/json", "text/event-stream"} {
		if !strings.Contains(initReq.accept, want) {
			t.Fatalf("Accept %q does not contain %q", initReq.accept, want)
		}
	}
	if initReq.sessionID != "" {
		t.Fatalf("initialize must not send a session id, got %q", initReq.sessionID)
	}

	notifyReq := requests[1]
	if notifyReq.method != "notifications/initialized" {
		t.Fatalf("second request method = %q", notifyReq.method)
	}
	if notifyReq.sessionID != "sess-remote-1" {
		t.Fatalf("notification session id = %q", notifyReq.sessionID)
	}
	if notifyReq.protocolVersion != "2025-03-26" {
		t.Fatalf("notification protocol header = %q", notifyReq.protocolVersion)
	}

	for _, req := range requests[2:] {
		if req.authorization != "Bearer "+syntheticToken {
			t.Fatalf("%s missing bearer token", req.method)
		}
		if req.sessionID != "sess-remote-1" {
			t.Fatalf("%s missing session id", req.method)
		}
	}
	if requests[3].method != "tools/call" || requests[3].protocolVersion != "2025-03-26" {
		t.Fatalf("tools/call request = %+v", requests[3])
	}
}

func TestA26SSEEventStreamParsing(t *testing.T) {
	server := newMCPServer(t)
	server.sse = true
	client, _ := newTestClient(t, server)
	ctx := context.Background()

	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize over SSE: %v", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	res, err := client.CallReadOnlyTool(ctx, "demo.read_status", nil)
	if err != nil {
		t.Fatalf("CallReadOnlyTool over SSE: %v", err)
	}
	if res.Text() != "synthetic read result" {
		t.Fatalf("SSE result = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// A26: 401, TLS verification, timeout, permission refusal.
// ---------------------------------------------------------------------------

func TestA26Unauthorized(t *testing.T) {
	t.Run("initialize with bad token", func(t *testing.T) {
		server := newMCPServer(t)
		ts := httptest.NewTLSServer(server.handler())
		t.Cleanup(ts.Close)
		client, err := remotemcp.New(remotemcp.Config{
			URL:         ts.URL,
			BearerToken: "wrong-synthetic-token",
			Timeout:     2 * time.Second,
			HTTPClient:  ts.Client(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = client.Initialize(context.Background())
		if !errors.Is(err, remotemcp.ErrUnauthorized) {
			t.Fatalf("Initialize error = %v, want ErrUnauthorized", err)
		}
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("401 was not surfaced clearly: %v", err)
		}
	})

	t.Run("list tools unauthorized after initialize", func(t *testing.T) {
		server := newMCPServer(t)
		server.statusByMeth["tools/list"] = http.StatusUnauthorized
		client, _ := newTestClient(t, server)
		ctx := context.Background()
		if _, err := client.Initialize(ctx); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if _, err := client.ListTools(ctx); !errors.Is(err, remotemcp.ErrUnauthorized) {
			t.Fatalf("ListTools error = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("forbidden target", func(t *testing.T) {
		server := newMCPServer(t)
		server.statusByMeth["tools/list"] = http.StatusForbidden
		client, _ := newTestClient(t, server)
		ctx := context.Background()
		if _, err := client.Initialize(ctx); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if _, err := client.ListTools(ctx); !errors.Is(err, remotemcp.ErrForbidden) {
			t.Fatalf("ListTools error = %v, want ErrForbidden", err)
		}
	})
}

func TestA26TLSVerificationFailure(t *testing.T) {
	server := newMCPServer(t)
	ts := httptest.NewTLSServer(server.handler())
	t.Cleanup(ts.Close)

	// A default HTTP client must fail certificate verification. The probe never
	// sets InsecureSkipVerify and never retries insecurely.
	client, err := remotemcp.New(remotemcp.Config{URL: ts.URL, BearerToken: syntheticToken, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Initialize(context.Background())
	if !errors.Is(err, remotemcp.ErrTLS) {
		t.Fatalf("Initialize error = %v, want ErrTLS", err)
	}
	var certErr x509.UnknownAuthorityError
	if !errors.As(err, &certErr) {
		t.Fatalf("certificate verification error is not in the chain: %v", err)
	}
	requests, _ := server.snapshot()
	if len(requests) != 0 {
		t.Fatalf("HTTP requests reached the server despite certificate failure: %+v", requests)
	}
}

func TestA26Timeout(t *testing.T) {
	server := newMCPServer(t)
	server.sleep = 500 * time.Millisecond
	ts := httptest.NewTLSServer(server.handler())
	t.Cleanup(ts.Close)
	client, err := remotemcp.New(remotemcp.Config{
		URL:         ts.URL,
		BearerToken: syntheticToken,
		Timeout:     25 * time.Millisecond,
		HTTPClient:  ts.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if _, err := client.Initialize(context.Background()); !errors.Is(err, remotemcp.ErrTimeout) {
		t.Fatalf("Initialize error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout not bounded: %s", elapsed)
	}
}

func TestA26RefusesWriteToolWithoutHTTPRequest(t *testing.T) {
	server := newMCPServer(t)
	client, _ := newTestClient(t, server)
	ctx := context.Background()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	before, called := server.snapshot()

	refused := []struct {
		name string
		want error
	}{
		{"demo.set_volume", remotemcp.ErrToolNotAllowed},
		{"demo.no_annotation", remotemcp.ErrToolNotAllowed},
		{"demo.not_discovered", remotemcp.ErrToolUnknown},
	}
	for _, tc := range refused {
		if _, err := client.CallReadOnlyTool(ctx, tc.name, nil); !errors.Is(err, tc.want) {
			t.Fatalf("CallReadOnlyTool(%q) error = %v, want %v", tc.name, err, tc.want)
		}
	}
	after, calledAfter := server.snapshot()
	if len(after) != len(before) {
		t.Fatalf("refused tools caused HTTP requests: before=%d after=%d", len(before), len(after))
	}
	if len(calledAfter) != len(called) {
		t.Fatalf("refused tools reached the server: %v", calledAfter)
	}

	// A read-only annotated tool is allowed without an allowlist.
	if _, err := client.CallReadOnlyTool(ctx, "demo.read_status", nil); err != nil {
		t.Fatalf("read-only call: %v", err)
	}

	// An allowlist cannot promote an explicitly write-annotated tool.
	server2 := newMCPServer(t)
	ts2 := httptest.NewTLSServer(server2.handler())
	t.Cleanup(ts2.Close)
	client2, err := remotemcp.New(remotemcp.Config{
		URL:         ts2.URL,
		BearerToken: server2.requireToken,
		Timeout:     2 * time.Second,
		HTTPClient:  ts2.Client(),
		AllowTools:  []string{"demo.set_volume"},
	})
	if err != nil {
		t.Fatalf("New allowlisted: %v", err)
	}
	if _, err := client2.Initialize(ctx); err != nil {
		t.Fatalf("Initialize allowlisted: %v", err)
	}
	if _, err := client2.ListTools(ctx); err != nil {
		t.Fatalf("ListTools allowlisted: %v", err)
	}
	before2, _ := server2.snapshot()
	if _, err := client2.CallReadOnlyTool(ctx, "demo.set_volume", nil); !errors.Is(err, remotemcp.ErrToolNotAllowed) {
		t.Fatalf("allowlisted write tool error = %v, want ErrToolNotAllowed", err)
	}
	after2, called2 := server2.snapshot()
	if len(after2) != len(before2) || len(called2) != 0 {
		t.Fatalf("allowlisted write tool reached the server: requests %d->%d calls=%v", len(before2), len(after2), called2)
	}

	// A real read-only allowlist permits exactly the listed tool.
	server3 := newMCPServer(t)
	ts3 := httptest.NewTLSServer(server3.handler())
	t.Cleanup(ts3.Close)
	client3, err := remotemcp.New(remotemcp.Config{
		URL:         ts3.URL,
		BearerToken: server3.requireToken,
		Timeout:     2 * time.Second,
		HTTPClient:  ts3.Client(),
		AllowTools:  []string{"demo.read_status"},
	})
	if err != nil {
		t.Fatalf("New allowlist read: %v", err)
	}
	if _, err := client3.Initialize(ctx); err != nil {
		t.Fatalf("Initialize allowlist read: %v", err)
	}
	if _, err := client3.ListTools(ctx); err != nil {
		t.Fatalf("ListTools allowlist read: %v", err)
	}
	if _, err := client3.CallReadOnlyTool(ctx, "demo.set_volume", nil); !errors.Is(err, remotemcp.ErrToolNotAllowed) {
		t.Fatalf("non-allowlisted tool error = %v, want ErrToolNotAllowed", err)
	}
	if _, err := client3.CallReadOnlyTool(ctx, "demo.read_status", nil); err != nil {
		t.Fatalf("allowlisted read-only call: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A26: JSON-RPC errors, isError, oversized responses, config validation.
// ---------------------------------------------------------------------------

func TestA26JSONRPCAndToolErrorsAreFailures(t *testing.T) {
	t.Run("JSON-RPC error", func(t *testing.T) {
		server := newMCPServer(t)
		server.rpcErrorCall = true
		client, _ := newTestClient(t, server)
		ctx := context.Background()
		if _, err := client.Initialize(ctx); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if _, err := client.ListTools(ctx); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		_, err := client.CallReadOnlyTool(ctx, "demo.read_status", nil)
		if !errors.Is(err, remotemcp.ErrRPC) {
			t.Fatalf("CallReadOnlyTool error = %v, want ErrRPC", err)
		}
		var rpcErr *remotemcp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
			t.Fatalf("RPC error details = %+v", err)
		}
	})

	t.Run("isError result", func(t *testing.T) {
		server := newMCPServer(t)
		server.isErrorCall = true
		client, _ := newTestClient(t, server)
		ctx := context.Background()
		if _, err := client.Initialize(ctx); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if _, err := client.ListTools(ctx); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		res, err := client.CallReadOnlyTool(ctx, "demo.read_status", nil)
		if !errors.Is(err, remotemcp.ErrToolError) {
			t.Fatalf("CallReadOnlyTool error = %v, want ErrToolError", err)
		}
		if !res.IsError || res.Text() != "synthetic tool failure" {
			t.Fatalf("isError result = %+v", res)
		}
	})
}

func TestA26OversizedResponseRejected(t *testing.T) {
	server := newMCPServer(t)
	server.hugeInit = true
	client, _ := newTestClient(t, server)
	if _, err := client.Initialize(context.Background()); !errors.Is(err, remotemcp.ErrTooLarge) {
		t.Fatalf("Initialize error = %v, want ErrTooLarge", err)
	}
}

func TestA26ConfigurationValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  remotemcp.Config
	}{
		{name: "empty URL", cfg: remotemcp.Config{}},
		{name: "plaintext URL", cfg: remotemcp.Config{URL: "http://example.invalid/mcp"}},
		{name: "URL without host", cfg: remotemcp.Config{URL: "https:///mcp"}},
		{name: "URL with credentials", cfg: remotemcp.Config{URL: "https://user:pass@example.invalid/mcp"}},
		{name: "negative timeout", cfg: remotemcp.Config{URL: "https://example.invalid/mcp", Timeout: -time.Second}},
		{name: "excessive timeout", cfg: remotemcp.Config{URL: "https://example.invalid/mcp", Timeout: limits.UpstreamOpMax + time.Second}},
		{name: "empty allowlist entry", cfg: remotemcp.Config{URL: "https://example.invalid/mcp", AllowTools: []string{""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := remotemcp.New(tc.cfg); !errors.Is(err, remotemcp.ErrConfig) {
				t.Fatalf("New error = %v, want ErrConfig", err)
			}
		})
	}
}

func TestA26RequiresInitializeAndHonorsClose(t *testing.T) {
	server := newMCPServer(t)
	client, _ := newTestClient(t, server)
	ctx := context.Background()
	if _, err := client.ListTools(ctx); !errors.Is(err, remotemcp.ErrNotInitialized) {
		t.Fatalf("ListTools before initialize = %v, want ErrNotInitialized", err)
	}
	if _, err := client.CallReadOnlyTool(ctx, "demo.read_status", nil); !errors.Is(err, remotemcp.ErrNotInitialized) {
		t.Fatalf("CallReadOnlyTool before initialize = %v, want ErrNotInitialized", err)
	}
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.Initialize(ctx); !errors.Is(err, remotemcp.ErrAlreadyInitialized) {
		t.Fatalf("duplicate Initialize = %v, want ErrAlreadyInitialized", err)
	}
	before, _ := server.snapshot()
	if got := len(before); got != 2 {
		t.Fatalf("server saw %d requests after initialize, want 2", got)
	}
	client.Close()
	if _, err := client.Initialize(ctx); !errors.Is(err, remotemcp.ErrClosed) {
		t.Fatalf("Initialize after Close = %v, want ErrClosed", err)
	}
	if _, err := client.ListTools(ctx); !errors.Is(err, remotemcp.ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}
}
