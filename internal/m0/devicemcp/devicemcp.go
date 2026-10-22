// Package devicemcp implements the M0 device MCP client from the approved
// M0-v0.1 protocol document (docs/milestones/M0-transport-probe, section 6).
//
// A device MCP exchange is one MQTT application message of the form
//
//	{"type": "mcp", "payload": <JSON-RPC 2.0 message>}
//
// The MQTT wrapper and the authenticated connection are the only identity
// source. The client is therefore bound at construction to an authenticated
// device session identity (device, connection generation, protocol session).
// Every request id is correlated with that full identity, and a response only
// completes a request when device, generation, session and JSON-RPC id all
// match. Duplicate, late, other-device and old-generation/session responses
// are ignored without side effects.
//
// JSON-RPC error responses and tool results with isError=true are failures.
// They are never converted into success. All requests are bounded by a
// timeout (limits.MCPRequestTimeout unless configured otherwise) and all
// message sizes by limits.MaxJSONBytes.
package devicemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/limits"
	"github.com/homevoice/homevoice/internal/m0/logging"
)

// JSON-RPC and MCP protocol constants for the synthetic probe.
const (
	// JSONRPCVersion is the only JSON-RPC version accepted.
	JSONRPCVersion = "2.0"

	// ProtocolVersion is the MCP protocol version this probe requests. The
	// value is a synthetic candidate and must be confirmed against real
	// firmware (H05); it is never assumed.
	ProtocolVersion = "2024-11-05"

	// ClientName and ClientVersion identify the probe during initialize.
	ClientName    = "homevoice-m0"
	ClientVersion = "0.1.0"

	// Methods used by the probe.
	MethodInitialize       = "initialize"
	MethodInitialized      = "notifications/initialized"
	MethodListTools        = "tools/list"
	MethodCallTool         = "tools/call"
	MethodToolsListChanged = "notifications/tools/list_changed"

	maxToolNameBytes   = 128
	maxServerInfoBytes = 256
	maxToolDescBytes   = 4096
)

// DefaultRequestTimeout is the bounded per-request timeout.
const DefaultRequestTimeout = limits.MCPRequestTimeout

// Errors returned by the client. Callers distinguish the bounded failure
// classes with errors.Is.
var (
	// ErrTimeout reports a request that exceeded its bounded timeout.
	ErrTimeout = errors.New("device MCP request timed out")

	// ErrCanceled reports a request canceled by the caller, Cancel or Close.
	ErrCanceled = errors.New("device MCP request canceled")

	// ErrClosed reports a request on a closed client.
	ErrClosed = errors.New("device MCP client closed")

	// ErrAlreadyInitialized reports a duplicate initialize call; the
	// initialized notification is sent at most once.
	ErrAlreadyInitialized = errors.New("device MCP client already initialized")

	// ErrRPC reports a JSON-RPC error response (never a success).
	ErrRPC = errors.New("device MCP JSON-RPC error")

	// ErrToolError reports result.isError=true (never a success).
	ErrToolError = errors.New("device MCP tool returned an error result")

	// ErrInvalidResponse reports a malformed or unsupported response.
	ErrInvalidResponse = errors.New("device MCP invalid response")

	// ErrInvalidRequest reports a locally invalid request.
	ErrInvalidRequest = errors.New("device MCP invalid request")

	// ErrTooLarge reports an application JSON message over the probe limit.
	ErrTooLarge = errors.New("device MCP message too large")

	// ErrTooManyTools reports more than limits.MCPMaxTools discovered tools.
	ErrTooManyTools = errors.New("device MCP tool count exceeds limit")

	// ErrTooManyPages reports more than limits.MCPMaxPages tools/list pages.
	ErrTooManyPages = errors.New("device MCP tools/list page limit exceeded")

	// ErrCursorCycle reports a repeated tools/list pagination cursor.
	ErrCursorCycle = errors.New("device MCP tools/list cursor cycle detected")

	// ErrSend reports a failure from the underlying MQTT sender.
	ErrSend = errors.New("device MCP send failed")
)

// Identity is the authenticated device session a client is bound to. It is
// never read from a payload.
type Identity struct {
	DeviceID   string
	Generation uint64
	SessionID  string
}

// Sender sends one JSON-RPC payload as an MQTT application message. The
// implementation wraps it as {"type":"mcp","payload":...} and publishes it on
// the authenticated connection's downlink topic.
type Sender interface {
	SendMCP(payload json.RawMessage) error
}

// Envelope is one inbound application message together with the trusted
// transport-supplied identity of the connection that delivered it.
type Envelope struct {
	DeviceID   string
	Generation uint64
	SessionID  string
	Payload    json.RawMessage
}

// Tool is one discovered device tool. InputSchema is the complete schema the
// device returned, preserved for the caller.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ServerInfo describes the device MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the parsed initialize response.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ServerInfo      ServerInfo      `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

// Content is one MCP tool result content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallResult is a tool call response. A non-nil error from CallTool always
// means failure, even when IsError is populated for diagnostics.
type CallResult struct {
	Content           []Content       `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	Raw               json.RawMessage `json:"-"`
}

// Text returns the concatenated text content blocks.
func (r CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error renders the error without the (potentially large) data field.
func (e *RPCError) Error() string {
	return fmt.Sprintf("device MCP JSON-RPC error %d: %s", e.Code, e.Message)
}

// Unwrap allows errors.Is(err, ErrRPC).
func (e *RPCError) Unwrap() error { return ErrRPC }

// Client is a device MCP client bound to one authenticated device session.
// It is safe for concurrent use by multiple goroutines.
type Client struct {
	identity Identity
	sender   Sender
	clk      clock.Clock
	logger   *slog.Logger

	mu           sync.Mutex
	nextID       int64
	pending      map[int64]*pending
	timeout      time.Duration
	closed       bool
	initializing bool
	initialized  bool
}

// pending is one in-flight request and the identity it was issued for.
type pending struct {
	identity Identity
	ch       chan outcome
}

// outcome is the single completion of a pending request.
type outcome struct {
	result json.RawMessage
	err    error
}

// New creates a client bound to id. A nil clock uses the real clock; a nil
// logger discards log records. The default request timeout is
// limits.MCPRequestTimeout.
func New(id Identity, sender Sender, clk clock.Clock, logger *slog.Logger) *Client {
	if clk == nil {
		clk = clock.Real{}
	}
	if logger == nil {
		logger = logging.Discard()
	}
	return &Client{
		identity: id,
		sender:   sender,
		clk:      clk,
		logger:   logger,
		pending:  make(map[int64]*pending),
		nextID:   1, // unique, increasing JSON-RPC ids within the session
		timeout:  limits.MCPRequestTimeout,
	}
}

// Identity returns the authenticated session identity the client is bound to.
func (c *Client) Identity() Identity { return c.identity }

// SetRequestTimeout configures the bounded per-request timeout. A value <= 0
// restores limits.MCPRequestTimeout.
func (c *Client) SetRequestTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		d = limits.MCPRequestTimeout
	}
	c.timeout = d
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

// Initialize performs MCP initialization and then sends
// notifications/initialized exactly once. The initialized notification is only
// sent after a successful initialize result.
func (c *Client) Initialize(ctx context.Context) (InitializeResult, error) {
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return InitializeResult{}, ErrClosed
	case c.initialized || c.initializing:
		c.mu.Unlock()
		return InitializeResult{}, ErrAlreadyInitialized
	}
	c.initializing = true
	c.mu.Unlock()

	finish := func(ok bool) {
		c.mu.Lock()
		c.initializing = false
		if ok {
			c.initialized = true
		}
		c.mu.Unlock()
	}

	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: ClientName, Version: ClientVersion},
	}
	raw, err := c.call(ctx, MethodInitialize, params)
	if err != nil {
		finish(false)
		return InitializeResult{}, err
	}
	res, err := parseInitializeResult(raw)
	if err != nil {
		finish(false)
		return InitializeResult{}, err
	}
	if err := c.notify(MethodInitialized, nil); err != nil {
		finish(false)
		return InitializeResult{}, err
	}
	finish(true)
	c.logger.Debug("device MCP initialized",
		"device_id", c.identity.DeviceID,
		"generation", c.identity.Generation,
		"session_id", c.identity.SessionID,
		"protocol_version", res.ProtocolVersion,
	)
	return res, nil
}

// ListTools discovers all device tools with cursor pagination bounded by
// limits.MCPMaxPages and limits.MCPMaxTools. Repeated cursors are rejected.
// Tool entries whose shape is unsupported (missing name, non-object schema)
// are not exposed; they are skipped rather than guessed at.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	seenNames := make(map[string]bool)
	seenCursors := make(map[string]bool)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= limits.MCPMaxPages {
			return nil, fmt.Errorf("%w: more than %d pages", ErrTooManyPages, limits.MCPMaxPages)
		}
		var params json.RawMessage
		if cursor == "" {
			params = json.RawMessage(`{}`)
		} else {
			raw, err := json.Marshal(struct {
				Cursor string `json:"cursor"`
			}{Cursor: cursor})
			if err != nil {
				return nil, fmt.Errorf("%w: marshal cursor: %v", ErrInvalidRequest, err)
			}
			params = raw
		}
		raw, err := c.call(ctx, MethodListTools, params)
		if err != nil {
			return nil, err
		}
		pageTools, nextCursor, err := parseToolsPage(raw)
		if err != nil {
			return nil, err
		}
		for _, rawTool := range pageTools {
			tool, ok := parseTool(rawTool)
			if !ok {
				c.logger.Debug("device MCP tool not exposed: unsupported shape",
					"device_id", c.identity.DeviceID)
				continue
			}
			if seenNames[tool.Name] {
				c.logger.Debug("device MCP duplicate tool skipped", "tool", tool.Name)
				continue
			}
			seenNames[tool.Name] = true
			tools = append(tools, tool)
			if len(tools) > limits.MCPMaxTools {
				return nil, fmt.Errorf("%w: more than %d tools", ErrTooManyTools, limits.MCPMaxTools)
			}
		}
		if nextCursor == "" {
			return tools, nil
		}
		if seenCursors[nextCursor] {
			return nil, fmt.Errorf("%w: cursor %q repeated", ErrCursorCycle, nextCursor)
		}
		seenCursors[nextCursor] = true
		cursor = nextCursor
	}
}

// CallTool invokes one device tool. A JSON-RPC error or result.isError=true is
// returned as an error, never as success.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if strings.TrimSpace(name) == "" {
		return CallResult{}, fmt.Errorf("%w: tool name is empty", ErrInvalidRequest)
	}
	if len(args) > 0 && !isJSONObject(args) {
		return CallResult{}, fmt.Errorf("%w: tool arguments must be a JSON object", ErrInvalidRequest)
	}
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Name: name, Arguments: args}
	raw, err := c.call(ctx, MethodCallTool, params)
	if err != nil {
		return CallResult{}, err
	}
	if !isJSONObject(raw) {
		return CallResult{}, fmt.Errorf("%w: tools/call result is not a JSON object", ErrInvalidResponse)
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallResult{}, fmt.Errorf("%w: tools/call result: %v", ErrInvalidResponse, err)
	}
	res.Raw = append(json.RawMessage(nil), raw...)
	if res.IsError {
		return res, fmt.Errorf("%w: %s", ErrToolError, res.Text())
	}
	return res, nil
}

// HandleMessage routes one inbound application message. It only completes a
// request when the trusted identity and the JSON-RPC id match a pending
// request; everything else (duplicates, late responses, other devices, old
// generations/sessions, malformed payloads) is ignored.
func (c *Client) HandleMessage(env Envelope) {
	if len(env.Payload) == 0 || len(env.Payload) > limits.MaxJSONBytes {
		c.logger.Debug("device MCP inbound message ignored: size",
			"device_id", env.DeviceID, "generation", env.Generation, "session_id", env.SessionID)
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(env.Payload, &msg); err != nil || msg.JSONRPC != JSONRPCVersion {
		c.logger.Debug("device MCP inbound message ignored: malformed",
			"device_id", env.DeviceID, "generation", env.Generation, "session_id", env.SessionID)
		return
	}
	id, hasID := parseID(msg.ID)
	if !hasID {
		if msg.Method != "" {
			c.logger.Debug("device MCP inbound notification ignored", "method", msg.Method)
		}
		return
	}
	if len(msg.Result) == 0 && len(msg.Error) == 0 {
		c.logger.Debug("device MCP inbound message ignored: no result or error", "id", id)
		return
	}

	envIdentity := Identity{DeviceID: env.DeviceID, Generation: env.Generation, SessionID: env.SessionID}
	c.mu.Lock()
	p, ok := c.pending[id]
	if !ok || p.identity != envIdentity {
		c.mu.Unlock()
		c.logger.Debug("device MCP stale/cross-session response ignored",
			"id", id,
			"device_id", env.DeviceID,
			"generation", env.Generation,
			"session_id", env.SessionID,
		)
		return
	}
	delete(c.pending, id)
	c.mu.Unlock()

	if len(msg.Error) > 0 {
		p.ch <- outcome{err: parseRPCError(msg.Error)}
		return
	}
	p.ch <- outcome{result: append(json.RawMessage(nil), msg.Result...)}
}

// Cancel fails every pending request with ErrCanceled. The client remains
// usable; late responses for canceled requests are ignored.
func (c *Client) Cancel() {
	c.failPending(ErrCanceled, false)
}

// Close fails every pending request with ErrCanceled and rejects new requests.
func (c *Client) Close() {
	c.failPending(ErrCanceled, true)
}

func (c *Client) failPending(err error, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if closed {
		c.closed = true
	}
	for id, p := range c.pending {
		delete(c.pending, id)
		p.ch <- outcome{err: err}
	}
}

// call sends one request and waits for its correlated response, timeout,
// cancellation or client shutdown.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.sender == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: nil sender", ErrInvalidRequest)
	}
	id := c.nextID
	c.nextID++
	p := &pending{identity: c.identity, ch: make(chan outcome, 1)}
	c.pending[id] = p
	timeout := c.timeout
	c.mu.Unlock()

	cleanup := func() {
		c.mu.Lock()
		if cur, ok := c.pending[id]; ok && cur == p {
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}

	req := rpcRequest{JSONRPC: JSONRPCVersion, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("%w: marshal params: %v", ErrInvalidRequest, err)
		}
		if len(raw) > limits.MaxJSONBytes {
			cleanup()
			return nil, fmt.Errorf("%w: params %d > %d bytes", ErrTooLarge, len(raw), limits.MaxJSONBytes)
		}
		req.Params = raw
	}
	payload, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: marshal request: %v", ErrInvalidRequest, err)
	}
	if len(payload) > limits.MaxJSONBytes {
		cleanup()
		return nil, fmt.Errorf("%w: request %d > %d bytes", ErrTooLarge, len(payload), limits.MaxJSONBytes)
	}

	// Register the timer before sending so the timeout bound holds even for a
	// sender that blocks, and so tests can advance a fake clock deterministically.
	timer := c.clk.After(timeout)
	if err := c.sender.SendMCP(payload); err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: %v", ErrSend, err)
	}
	select {
	case out := <-p.ch:
		return out.result, out.err
	case <-ctx.Done():
		cleanup()
		return nil, fmt.Errorf("%w: %v", ErrCanceled, ctx.Err())
	case <-timer:
		cleanup()
		return nil, fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
}

// notify sends one JSON-RPC notification (no id, no response expected).
func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	closed := c.closed
	sender := c.sender
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if sender == nil {
		return fmt.Errorf("%w: nil sender", ErrInvalidRequest)
	}
	msg := rpcNotification{JSONRPC: JSONRPCVersion, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("%w: marshal notification params: %v", ErrInvalidRequest, err)
		}
		msg.Params = raw
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%w: marshal notification: %v", ErrInvalidRequest, err)
	}
	if len(payload) > limits.MaxJSONBytes {
		return fmt.Errorf("%w: notification too large", ErrTooLarge)
	}
	if err := sender.SendMCP(payload); err != nil {
		return fmt.Errorf("%w: %v", ErrSend, err)
	}
	return nil
}

func parseInitializeResult(raw json.RawMessage) (InitializeResult, error) {
	if !isJSONObject(raw) {
		return InitializeResult{}, fmt.Errorf("%w: initialize result is not a JSON object", ErrInvalidResponse)
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return InitializeResult{}, fmt.Errorf("%w: initialize result: %v", ErrInvalidResponse, err)
	}
	if strings.TrimSpace(res.ProtocolVersion) == "" {
		return InitializeResult{}, fmt.Errorf("%w: initialize result has no protocolVersion", ErrInvalidResponse)
	}
	if len(res.ServerInfo.Name) > maxServerInfoBytes || len(res.ServerInfo.Version) > maxServerInfoBytes {
		return InitializeResult{}, fmt.Errorf("%w: initialize serverInfo too long", ErrInvalidResponse)
	}
	return res, nil
}

func parseToolsPage(raw json.RawMessage) ([]json.RawMessage, string, error) {
	if !isJSONObject(raw) {
		return nil, "", fmt.Errorf("%w: tools/list result is not a JSON object", ErrInvalidResponse)
	}
	var page struct {
		Tools      json.RawMessage `json:"tools"`
		NextCursor string          `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("%w: tools/list result: %v", ErrInvalidResponse, err)
	}
	if len(page.Tools) == 0 {
		return nil, "", fmt.Errorf("%w: tools/list result has no tools array", ErrInvalidResponse)
	}
	if !isJSONArray(page.Tools) {
		return nil, "", fmt.Errorf("%w: tools/list tools is not an array", ErrInvalidResponse)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(page.Tools, &tools); err != nil {
		return nil, "", fmt.Errorf("%w: tools/list tools: %v", ErrInvalidResponse, err)
	}
	return tools, page.NextCursor, nil
}

func parseTool(raw json.RawMessage) (Tool, bool) {
	if !isJSONObject(raw) {
		return Tool{}, false
	}
	var parsed struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Tool{}, false
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" || len(name) > maxToolNameBytes {
		return Tool{}, false
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return Tool{}, false
	}
	if len(parsed.Description) > maxToolDescBytes {
		return Tool{}, false
	}
	if !isJSONObject(parsed.InputSchema) {
		return Tool{}, false
	}
	return Tool{
		Name:        name,
		Description: parsed.Description,
		InputSchema: append(json.RawMessage(nil), parsed.InputSchema...),
	}, true
}

func parseRPCError(raw json.RawMessage) error {
	var parsed struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || !isJSONObject(raw) {
		return fmt.Errorf("%w: malformed JSON-RPC error object", ErrInvalidResponse)
	}
	return &RPCError{Code: parsed.Code, Message: parsed.Message, Data: parsed.Data}
}

// parseID accepts a JSON number id (as used by this client) and rejects
// strings/objects: the probe's own ids are always integers.
func parseID(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	if s[0] == '"' || s[0] == '{' || s[0] == '[' {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func isJSONObject(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}'
}

func isJSONArray(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']'
}
