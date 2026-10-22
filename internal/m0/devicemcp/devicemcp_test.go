package devicemcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/devicemcp"
	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
)

// fakeSender records every JSON-RPC payload sent over the MQTT application
// wrapper and, when a handler is installed, delivers synthetic responses
// synchronously (which exercises the same HandleMessage path a real transport
// would use).
type fakeSender struct {
	mu      sync.Mutex
	sent    []json.RawMessage
	err     error
	handler func(payload json.RawMessage)
	notify  chan struct{}
}

func newFakeSender() *fakeSender {
	return &fakeSender{notify: make(chan struct{}, 256)}
}

func (s *fakeSender) SendMCP(payload json.RawMessage) error {
	cp := append(json.RawMessage(nil), payload...)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return s.err
	}
	s.sent = append(s.sent, cp)
	handler := s.handler
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	if handler != nil {
		handler(cp)
	}
	return nil
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *fakeSender) messages() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *fakeSender) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s.count() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d sent messages, have %d", n, s.count())
		}
		select {
		case <-s.notify:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

var idA = devicemcp.Identity{DeviceID: "dev-a", Generation: 1, SessionID: "sess-a"}

func envFor(id devicemcp.Identity, payload json.RawMessage) devicemcp.Envelope {
	return devicemcp.Envelope{
		DeviceID:   id.DeviceID,
		Generation: id.Generation,
		SessionID:  id.SessionID,
		Payload:    payload,
	}
}

func newClient(t *testing.T, id devicemcp.Identity) (*devicemcp.Client, *fakeSender, *clock.Fake) {
	t.Helper()
	sender := newFakeSender()
	clk := clock.NewFake(time.Unix(1700000000, 0))
	c := devicemcp.New(id, sender, clk, logging.Discard())
	return c, sender, clk
}

func parseRequest(t *testing.T, payload json.RawMessage) (method string, id int64, hasID bool) {
	t.Helper()
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("sent payload is not JSON: %v (%s)", err, payload)
	}
	if msg.JSONRPC != devicemcp.JSONRPCVersion {
		t.Fatalf("sent payload jsonrpc = %q, want %q", msg.JSONRPC, devicemcp.JSONRPCVersion)
	}
	if len(msg.ID) == 0 {
		return msg.Method, 0, false
	}
	var parsedID int64
	if err := json.Unmarshal(msg.ID, &parsedID); err != nil {
		t.Fatalf("sent request id is not an integer: %v", err)
	}
	return msg.Method, parsedID, true
}

func respondResult(t *testing.T, c *devicemcp.Client, env devicemcp.Envelope, id int64, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	respondRawResult(t, c, env, id, raw)
}

func respondRawResult(t *testing.T, c *devicemcp.Client, env devicemcp.Envelope, id int64, result json.RawMessage) {
	t.Helper()
	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: devicemcp.JSONRPCVersion, ID: id, Result: result})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	c.HandleMessage(envFor(envIdentity(env), payload))
}

func envIdentity(env devicemcp.Envelope) devicemcp.Identity {
	return devicemcp.Identity{DeviceID: env.DeviceID, Generation: env.Generation, SessionID: env.SessionID}
}

func respondError(t *testing.T, c *devicemcp.Client, env devicemcp.Envelope, id int64, code int, message string) {
	t.Helper()
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%q}}`, id, code, message)
	c.HandleMessage(envFor(envIdentity(env), json.RawMessage(payload)))
}

func installAutoResponder(t *testing.T, c *devicemcp.Client, sender *fakeSender, respond func(method string, id int64)) {
	t.Helper()
	sender.handler = func(payload json.RawMessage) {
		method, id, hasID := parseRequest(t, payload)
		if !hasID {
			return
		}
		respond(method, id)
	}
}

func initializeOK(t *testing.T, c *devicemcp.Client, env devicemcp.Envelope, id int64) {
	t.Helper()
	respondResult(t, c, env, id, map[string]any{
		"protocolVersion": devicemcp.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "synthetic-device", "version": "0.0.9"},
	})
}

// controlBody returns the full MQTT application-message fixture body by id.
func controlBody(t *testing.T, id string) []byte {
	t.Helper()
	suite, err := fixtures.LoadControlMessages()
	if err != nil {
		t.Fatalf("load control fixtures: %v", err)
	}
	body, err := suite.Body(id)
	if err != nil {
		t.Fatalf("fixture %q: %v", id, err)
	}
	return body
}

// controlPayload returns the JSON-RPC payload of an MCP control fixture.
func controlPayload(t *testing.T, id string) json.RawMessage {
	t.Helper()
	var wrapper struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(controlBody(t, id), &wrapper); err != nil {
		t.Fatalf("fixture %q wrapper: %v", id, err)
	}
	if wrapper.Type != "mcp" {
		t.Fatalf("fixture %q type = %q, want mcp", id, wrapper.Type)
	}
	return wrapper.Payload
}

// controlResult returns the JSON-RPC result of a server-to-device fixture.
func controlResult(t *testing.T, id string) json.RawMessage {
	t.Helper()
	var payload struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(controlPayload(t, id), &payload); err != nil {
		t.Fatalf("fixture %q payload: %v", id, err)
	}
	if len(payload.Result) == 0 {
		t.Fatalf("fixture %q has no result", id)
	}
	return payload.Result
}

func jsonSemanticallyEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	decode := func(raw []byte) any {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode JSON %q: %v", raw, err)
		}
		return v
	}
	return reflect.DeepEqual(decode(got), decode(want))
}

func fixtureClient(t *testing.T) (*devicemcp.Client, *fakeSender) {
	t.Helper()
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		switch method {
		case devicemcp.MethodInitialize:
			initializeOK(t, c, env, id)
		case devicemcp.MethodListTools:
			respondRawResult(t, c, env, id, controlResult(t, "mcp-list-response"))
		case devicemcp.MethodCallTool:
			respondRawResult(t, c, env, id, controlResult(t, "mcp-tool-error"))
		}
	})
	return c, sender
}

// ---------------------------------------------------------------------------
// A21: initialization, initialized notification, pagination and bounds.
// ---------------------------------------------------------------------------

func TestA21InitializeSendsClientInfoAndNotificationOnce(t *testing.T) {
	c, sender := fixtureClient(t)

	res, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if res.ProtocolVersion != devicemcp.ProtocolVersion {
		t.Fatalf("protocol version = %q", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "synthetic-device" || res.ServerInfo.Version != "0.0.9" {
		t.Fatalf("server info = %+v", res.ServerInfo)
	}

	msgs := sender.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want initialize request + initialized notification", len(msgs))
	}
	// The initialize request must match the synthetic protocol fixture: it
	// carries protocolVersion, capabilities and clientInfo.
	if !jsonSemanticallyEqual(t, msgs[0], controlPayload(t, "mcp-initialize")) {
		t.Fatalf("initialize request differs from fixture\n got: %s\nwant: %s", msgs[0], controlPayload(t, "mcp-initialize"))
	}
	method, _, hasID := parseRequest(t, msgs[1])
	if method != devicemcp.MethodInitialized {
		t.Fatalf("second message method = %q, want %q", method, devicemcp.MethodInitialized)
	}
	if hasID {
		t.Fatalf("initialized notification must not carry an id: %s", msgs[1])
	}

	// The initialized notification is sent exactly once: a second Initialize
	// is rejected and does not emit another notification.
	if _, err := c.Initialize(context.Background()); !errors.Is(err, devicemcp.ErrAlreadyInitialized) {
		t.Fatalf("second Initialize error = %v, want ErrAlreadyInitialized", err)
	}
	if got := sender.count(); got != 2 {
		t.Fatalf("after duplicate Initialize sent %d messages, want 2", got)
	}
}

func TestA21ToolsListPagination(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	page := 0
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method != devicemcp.MethodListTools {
			return
		}
		page++
		switch page {
		case 1:
			respondResult(t, c, env, id, map[string]any{
				"tools": []any{
					map[string]any{"name": "demo.first", "description": "first", "inputSchema": map[string]any{"type": "object"}},
				},
				"nextCursor": "page-2",
			})
		case 2:
			respondRawResult(t, c, env, id, controlResult(t, "mcp-list-response"))
		default:
			t.Fatalf("unexpected tools/list page %d", page)
		}
	})

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].Name != "demo.first" || tools[1].Name != "demo.speaker.set_volume" {
		t.Fatalf("tool names = %q, %q", tools[0].Name, tools[1].Name)
	}
	// The complete demo inputSchema is preserved.
	var expected struct {
		Result struct {
			Tools []struct {
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(controlPayload(t, "mcp-list-response"), &expected); err != nil {
		t.Fatalf("fixture payload: %v", err)
	}
	if !jsonSemanticallyEqual(t, tools[1].InputSchema, expected.Result.Tools[0].InputSchema) {
		t.Fatalf("inputSchema not preserved\n got: %s\nwant: %s", tools[1].InputSchema, expected.Result.Tools[0].InputSchema)
	}
	for _, field := range []string{"minimum", "maximum", "required", "additionalProperties"} {
		if !strings.Contains(string(tools[1].InputSchema), field) {
			t.Fatalf("inputSchema lost %q: %s", field, tools[1].InputSchema)
		}
	}

	// The first tools/list request must carry the same method/params as the
	// protocol fixture (the fixture uses id 2 because it follows initialize;
	// this client was never initialized, so its first id is 1).
	msgs := sender.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d tools/list requests, want 2", len(msgs))
	}
	var gotRequest, wantRequest struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msgs[0], &gotRequest); err != nil {
		t.Fatalf("first page request: %v", err)
	}
	if err := json.Unmarshal(controlPayload(t, "mcp-list-request"), &wantRequest); err != nil {
		t.Fatalf("fixture list request: %v", err)
	}
	if gotRequest.Method != wantRequest.Method || !jsonSemanticallyEqual(t, gotRequest.Params, wantRequest.Params) {
		t.Fatalf("tools/list request method/params differ from fixture: %s", msgs[0])
	}
	var second struct {
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msgs[1], &second); err != nil {
		t.Fatalf("second page request: %v", err)
	}
	if second.Params.Cursor != "page-2" {
		t.Fatalf("second page cursor = %q, want page-2", second.Params.Cursor)
	}
}

func TestA21CursorCycleRejected(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	page := 0
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method != devicemcp.MethodListTools {
			return
		}
		page++
		respondResult(t, c, env, id, map[string]any{
			"tools":      []any{map[string]any{"name": fmt.Sprintf("demo.p%d", page), "inputSchema": map[string]any{"type": "object"}}},
			"nextCursor": "looping-cursor",
		})
	})
	_, err := c.ListTools(context.Background())
	if !errors.Is(err, devicemcp.ErrCursorCycle) {
		t.Fatalf("ListTools error = %v, want ErrCursorCycle", err)
	}
	if page != 2 {
		t.Fatalf("requested %d pages before cycle detection, want 2", page)
	}
}

func TestA21PageLimitRejected(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	page := 0
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method != devicemcp.MethodListTools {
			return
		}
		page++
		respondResult(t, c, env, id, map[string]any{
			"tools":      []any{map[string]any{"name": fmt.Sprintf("demo.p%d", page), "inputSchema": map[string]any{"type": "object"}}},
			"nextCursor": fmt.Sprintf("cursor-%d", page),
		})
	})
	_, err := c.ListTools(context.Background())
	if !errors.Is(err, devicemcp.ErrTooManyPages) {
		t.Fatalf("ListTools error = %v, want ErrTooManyPages", err)
	}
	if page != limits.MCPMaxPages {
		t.Fatalf("sent %d page requests, want exactly %d", page, limits.MCPMaxPages)
	}
}

func TestA21ToolLimitRejected(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method != devicemcp.MethodListTools {
			return
		}
		tools := make([]any, 0, limits.MCPMaxTools+1)
		for i := 0; i <= limits.MCPMaxTools; i++ {
			tools = append(tools, map[string]any{
				"name":        fmt.Sprintf("demo.tool.%d", i),
				"inputSchema": map[string]any{"type": "object"},
			})
		}
		respondResult(t, c, env, id, map[string]any{"tools": tools})
	})
	_, err := c.ListTools(context.Background())
	if !errors.Is(err, devicemcp.ErrTooManyTools) {
		t.Fatalf("ListTools error = %v, want ErrTooManyTools", err)
	}
}

func TestA21OversizedResponseRejected(t *testing.T) {
	c, sender, clk := newClient(t, idA)
	c.SetRequestTimeout(2 * time.Second)

	type listOutcome struct {
		tools []devicemcp.Tool
		err   error
	}
	done := make(chan listOutcome, 1)
	go func() {
		tools, err := c.ListTools(context.Background())
		done <- listOutcome{tools: tools, err: err}
	}()
	sender.waitCount(t, 1)

	oversized := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[],"pad":"` +
		strings.Repeat("x", limits.MaxJSONBytes) + `"}}`)
	if len(oversized) <= limits.MaxJSONBytes {
		t.Fatalf("test response is not oversized: %d bytes", len(oversized))
	}
	c.HandleMessage(envFor(idA, oversized))

	clk.Advance(2 * time.Second)
	select {
	case out := <-done:
		if !errors.Is(out.err, devicemcp.ErrTimeout) {
			t.Fatalf("ListTools error = %v, want ErrTimeout (oversized response must not complete)", out.err)
		}
		if out.tools != nil {
			t.Fatalf("oversized response yielded tools: %+v", out.tools)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListTools did not return after timeout")
	}
}

func TestA21NonObjectToolsResultRejected(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method == devicemcp.MethodListTools {
			respondResult(t, c, env, id, []any{"not-an-object"})
		}
	})
	if _, err := c.ListTools(context.Background()); !errors.Is(err, devicemcp.ErrInvalidResponse) {
		t.Fatalf("ListTools error = %v, want ErrInvalidResponse", err)
	}
}

// ---------------------------------------------------------------------------
// A22: request correlation, timeouts, duplicates, stale sessions.
// ---------------------------------------------------------------------------

func TestA22IdentityCorrelation(t *testing.T) {
	c, sender, _ := newClient(t, idA)

	type listOutcome struct {
		tools []devicemcp.Tool
		err   error
	}
	done := make(chan listOutcome, 1)
	go func() {
		tools, err := c.ListTools(context.Background())
		done <- listOutcome{tools: tools, err: err}
	}()
	sender.waitCount(t, 1)

	// Responses with the right id but the wrong trusted identity must not
	// complete the request.
	otherDevice := devicemcp.Identity{DeviceID: "dev-b", Generation: 1, SessionID: "sess-a"}
	oldGeneration := devicemcp.Identity{DeviceID: "dev-a", Generation: 0, SessionID: "sess-a"}
	oldSession := devicemcp.Identity{DeviceID: "dev-a", Generation: 1, SessionID: "sess-old"}
	stale := json.RawMessage(`{"tools":[{"name":"stale.tool","inputSchema":{"type":"object"}}]}`)
	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: devicemcp.JSONRPCVersion, ID: 1, Result: stale})
	if err != nil {
		t.Fatalf("marshal stale response: %v", err)
	}
	for _, staleID := range []devicemcp.Identity{otherDevice, oldGeneration, oldSession} {
		c.HandleMessage(envFor(staleID, payload))
	}

	// The correct identity completes the pending request exactly once.
	fresh := json.RawMessage(`{"tools":[{"name":"fresh.tool","inputSchema":{"type":"object"}}]}`)
	respondRawResult(t, c, envFor(idA, nil), 1, fresh)

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("ListTools: %v", out.err)
		}
		if len(out.tools) != 1 || out.tools[0].Name != "fresh.tool" {
			t.Fatalf("tools = %+v, want fresh.tool only (stale identity responses completed the request)", out.tools)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListTools did not return")
	}

	// A duplicate of the accepted response is ignored without side effects.
	respondRawResult(t, c, envFor(idA, nil), 1, fresh)
}

func TestA22TimeoutLateResponseAndDuplicateIgnored(t *testing.T) {
	c, sender, clk := newClient(t, idA)
	c.SetRequestTimeout(1 * time.Second)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.ListTools(context.Background())
		errCh <- err
	}()
	sender.waitCount(t, 1)
	clk.Advance(1 * time.Second)
	select {
	case err := <-errCh:
		if !errors.Is(err, devicemcp.ErrTimeout) {
			t.Fatalf("first ListTools error = %v, want ErrTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first ListTools did not time out")
	}

	// A late response for the timed-out request is ignored: it must not be
	// attributed to any later request.
	late := json.RawMessage(`{"tools":[{"name":"late.tool","inputSchema":{"type":"object"}}]}`)
	respondRawResult(t, c, envFor(idA, nil), 1, late)

	type listOutcome struct {
		tools []devicemcp.Tool
		err   error
	}
	done := make(chan listOutcome, 1)
	go func() {
		tools, err := c.ListTools(context.Background())
		done <- listOutcome{tools: tools, err: err}
	}()
	sender.waitCount(t, 2)
	respondRawResult(t, c, envFor(idA, nil), 2, json.RawMessage(`{"tools":[{"name":"second.tool","inputSchema":{"type":"object"}}]}`))
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("second ListTools: %v", out.err)
		}
		if len(out.tools) != 1 || out.tools[0].Name != "second.tool" {
			t.Fatalf("second ListTools tools = %+v, want second.tool", out.tools)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second ListTools did not return")
	}

	// Late duplicate of the already-completed second response is ignored.
	respondRawResult(t, c, envFor(idA, nil), 2, json.RawMessage(`{"tools":[{"name":"second.tool","inputSchema":{"type":"object"}}]}`))
}

func TestA22SameRequestIDOnTwoDevicesDoesNotCross(t *testing.T) {
	idB := devicemcp.Identity{DeviceID: "dev-b", Generation: 1, SessionID: "sess-b"}
	clientA, senderA, _ := newClient(t, idA)
	clientB, senderB, _ := newClient(t, idB)

	type listOutcome struct {
		tools []devicemcp.Tool
		err   error
	}
	doneA := make(chan listOutcome, 1)
	doneB := make(chan listOutcome, 1)
	go func() {
		tools, err := clientA.ListTools(context.Background())
		doneA <- listOutcome{tools: tools, err: err}
	}()
	go func() {
		tools, err := clientB.ListTools(context.Background())
		doneB <- listOutcome{tools: tools, err: err}
	}()
	senderA.waitCount(t, 1)
	senderB.waitCount(t, 1)

	// Both clients use request id 1. A response addressed as device A must not
	// complete device B's request.
	envA := envFor(idA, nil)
	respondRawResult(t, clientA, envA, 1, json.RawMessage(`{"tools":[{"name":"a.tool","inputSchema":{"type":"object"}}]}`))
	clientB.HandleMessage(envFor(idA, json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"cross.tool","inputSchema":{"type":"object"}}]}}`)))

	select {
	case out := <-doneA:
		if out.err != nil || len(out.tools) != 1 || out.tools[0].Name != "a.tool" {
			t.Fatalf("device A outcome = %+v, %v", out.tools, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("device A ListTools did not return")
	}

	respondRawResult(t, clientB, envFor(idB, nil), 1, json.RawMessage(`{"tools":[{"name":"b.tool","inputSchema":{"type":"object"}}]}`))
	select {
	case out := <-doneB:
		if out.err != nil || len(out.tools) != 1 || out.tools[0].Name != "b.tool" {
			t.Fatalf("device B outcome = %+v, %v", out.tools, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("device B ListTools did not return")
	}
}

func TestA22CancelFailsPendingAndLateResponseIgnored(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	errCh := make(chan error, 1)
	go func() {
		_, err := c.ListTools(context.Background())
		errCh <- err
	}()
	sender.waitCount(t, 1)
	c.Cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, devicemcp.ErrCanceled) {
			t.Fatalf("ListTools error = %v, want ErrCanceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListTools did not return after Cancel")
	}
	respondRawResult(t, c, envFor(idA, nil), 1, json.RawMessage(`{"tools":[]}`))

	// The client remains usable after Cancel.
	c2OK, sender2, _ := newClient(t, idA)
	data := make(chan struct {
		tools []devicemcp.Tool
		err   error
	}, 1)
	go func() {
		tools, err := c2OK.ListTools(context.Background())
		data <- struct {
			tools []devicemcp.Tool
			err   error
		}{tools, err}
	}()
	sender2.waitCount(t, 1)
	respondRawResult(t, c2OK, envFor(idA, nil), 1, json.RawMessage(`{"tools":[{"name":"after.cancel","inputSchema":{"type":"object"}}]}`))
	select {
	case out := <-data:
		if out.err != nil || len(out.tools) != 1 || out.tools[0].Name != "after.cancel" {
			t.Fatalf("post-cancel outcome = %+v, %v", out.tools, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-cancel ListTools did not return")
	}
}

// ---------------------------------------------------------------------------
// A23: JSON-RPC errors, isError, schema preservation, unsupported tools.
// ---------------------------------------------------------------------------

func TestA23JSONRPCErrorIsFailure(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method == devicemcp.MethodCallTool {
			respondError(t, c, env, id, -32602, "invalid params")
		}
	})
	res, err := c.CallTool(context.Background(), "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`))
	if !errors.Is(err, devicemcp.ErrRPC) {
		t.Fatalf("CallTool error = %v, want ErrRPC", err)
	}
	var rpcErr *devicemcp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("RPC error details = %+v", err)
	}
	if res.IsError {
		t.Fatalf("JSON-RPC error result must not report a tool success: %+v", res)
	}
}

func TestA23ToolErrorResultIsFailure(t *testing.T) {
	c, sender := fixtureClient(t)
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	res, err := c.CallTool(ctx, "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`))
	if !errors.Is(err, devicemcp.ErrToolError) {
		t.Fatalf("CallTool error = %v, want ErrToolError", err)
	}
	if !res.IsError {
		t.Fatalf("CallResult.IsError = false, want true")
	}
	if got := res.Text(); got != "合成执行失败" {
		t.Fatalf("tool error text = %q", got)
	}
	if !strings.Contains(err.Error(), "合成执行失败") {
		t.Fatalf("error does not surface tool text: %v", err)
	}

	// The whole fixture flow must reproduce the protocol's request bytes:
	// initialize id=1, notification, tools/list id=2, tools/call id=3.
	msgs := sender.messages()
	if len(msgs) != 4 {
		t.Fatalf("sent %d messages, want 4", len(msgs))
	}
	if !jsonSemanticallyEqual(t, msgs[2], controlPayload(t, "mcp-list-request")) {
		t.Fatalf("tools/list request differs from fixture: %s", msgs[2])
	}
	if !jsonSemanticallyEqual(t, msgs[3], controlPayload(t, "mcp-volume-call")) {
		t.Fatalf("tools/call request differs from fixture\n got: %s\nwant: %s", msgs[3], controlPayload(t, "mcp-volume-call"))
	}
}

func TestA23UnsupportedToolsNotExposed(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method != devicemcp.MethodListTools {
			return
		}
		respondResult(t, c, env, id, map[string]any{
			"tools": []any{
				map[string]any{"name": "", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "demo.bad-schema", "inputSchema": "not-an-object"},
				map[string]any{"name": "demo.no-schema"},
				map[string]any{"name": "demo.ok", "description": "ok", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "demo.ok", "inputSchema": map[string]any{"type": "object"}},
			},
		})
	})
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("exposed %d tools, want only the supported one: %+v", len(tools), tools)
	}
	if tools[0].Name != "demo.ok" {
		t.Fatalf("exposed tool = %q, want demo.ok", tools[0].Name)
	}
}

func TestA23CallToolNonObjectResultRejected(t *testing.T) {
	c, sender, _ := newClient(t, idA)
	env := envFor(idA, nil)
	installAutoResponder(t, c, sender, func(method string, id int64) {
		if method == devicemcp.MethodCallTool {
			respondResult(t, c, env, id, "ok")
		}
	})
	if _, err := c.CallTool(context.Background(), "demo.speaker.set_volume", json.RawMessage(`{"volume":20}`)); !errors.Is(err, devicemcp.ErrInvalidResponse) {
		t.Fatalf("CallTool error = %v, want ErrInvalidResponse", err)
	}
}

func TestA23SenderFailureDoesNotComplete(t *testing.T) {
	sender := newFakeSender()
	sender.err = errors.New("broker unavailable")
	c := devicemcp.New(idA, sender, clock.NewFake(time.Unix(0, 0)), logging.Discard())
	if _, err := c.CallTool(context.Background(), "demo.speaker.set_volume", json.RawMessage(`{"volume":1}`)); !errors.Is(err, devicemcp.ErrSend) {
		t.Fatalf("CallTool error = %v, want ErrSend", err)
	}
}
