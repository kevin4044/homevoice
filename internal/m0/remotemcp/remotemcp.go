// Package remotemcp implements the independent remote MCP probe from the M0
// specification (docs/milestones/M0-transport-probe, section 6 / R11).
//
// The M0 transport choice is Streamable HTTP JSON-RPC over HTTPS: every
// request is an HTTP POST with "Content-Type: application/json" and
// "Accept: application/json, text/event-stream"; a response body may be a
// single JSON-RPC message or a text/event-stream carrying one or more
// data events.
//
// The package is independent of the device MCP client. It never disables TLS
// certificate verification, bounds response size and request time, surfaces
// 401 and certificate failures clearly, and refuses to call write tools
// locally without sending an HTTP request. Reads are limited to tools the
// operator allowlists or that the server annotates with readOnlyHint=true.
// M0 never controls a real Home Assistant instance.
package remotemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// JSON-RPC and probe protocol constants.
const (
	// JSONRPCVersion is the only JSON-RPC version accepted.
	JSONRPCVersion = "2.0"

	// ClientProtocolVersion is the MCP protocol version requested by the
	// remote probe (Streamable HTTP). Real endpoints must be confirmed under
	// L03 authorization; nothing here is a firmware claim.
	ClientProtocolVersion = "2025-03-26"

	// ClientName and ClientVersion identify the probe during initialize.
	ClientName    = "homevoice-m0"
	ClientVersion = "0.1.0"

	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodListTools   = "tools/list"
	methodCallTool    = "tools/call"

	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
)

// maxResponseBytes bounds the HTTP response body read for one request.
// Remote MCP traffic is probe-only and never streams audio or private data.
const maxResponseBytes = limits.MaxJSONBytes

// maxSSEEvents bounds the number of data events parsed from one response.
const maxSSEEvents = 64

// maxErrorSnippetBytes bounds response text echoed in returned errors.
const maxErrorSnippetBytes = 256

// Errors returned by the client. Callers distinguish failure classes with
// errors.Is.
var (
	// ErrConfig reports an invalid URL, timeout or allowlist.
	ErrConfig = errors.New("remote MCP invalid configuration")

	// ErrClosed reports use of a closed client.
	ErrClosed = errors.New("remote MCP client closed")

	// ErrAlreadyInitialized reports a duplicate initialize call.
	ErrAlreadyInitialized = errors.New("remote MCP client already initialized")

	// ErrNotInitialized reports a tools request before initialize.
	ErrNotInitialized = errors.New("remote MCP client not initialized")

	// ErrUnauthorized reports HTTP 401 from the remote endpoint.
	ErrUnauthorized = errors.New("remote MCP unauthorized (HTTP 401)")

	// ErrForbidden reports HTTP 403 from the remote endpoint.
	ErrForbidden = errors.New("remote MCP forbidden (HTTP 403)")

	// ErrTLS reports a TLS certificate verification failure. The client never
	// retries insecurely.
	ErrTLS = errors.New("remote MCP TLS certificate verification failed")

	// ErrTimeout reports a request that exceeded its bounded timeout.
	ErrTimeout = errors.New("remote MCP request timed out")

	// ErrCanceled reports a request canceled by the caller.
	ErrCanceled = errors.New("remote MCP request canceled")

	// ErrNetwork reports a transport failure other than TLS/timeout.
	ErrNetwork = errors.New("remote MCP network failure")

	// ErrProtocol reports a malformed, oversized or unsupported HTTP/JSON-RPC
	// response.
	ErrProtocol = errors.New("remote MCP protocol error")

	// ErrRPC reports a JSON-RPC error response (never a success).
	ErrRPC = errors.New("remote MCP JSON-RPC error")

	// ErrToolError reports result.isError=true (never a success).
	ErrToolError = errors.New("remote MCP tool returned an error result")

	// ErrToolNotAllowed reports a tool refused locally as not read-only; no
	// HTTP request is sent.
	ErrToolNotAllowed = errors.New("remote MCP tool refused locally (not read-only)")

	// ErrToolUnknown reports a tool that was not discovered via tools/list.
	ErrToolUnknown = errors.New("remote MCP tool was not discovered via tools/list")

	// ErrInvalidRequest reports locally invalid arguments.
	ErrInvalidRequest = errors.New("remote MCP invalid request")

	// ErrTooLarge reports a response over the bounded size.
	ErrTooLarge = errors.New("remote MCP response too large")

	// ErrTooManyPages reports more than limits.MCPMaxPages tools/list pages.
	ErrTooManyPages = errors.New("remote MCP tools/list page limit exceeded")

	// ErrTooManyTools reports more than limits.MCPMaxTools discovered tools.
	ErrTooManyTools = errors.New("remote MCP tool count exceeds limit")

	// ErrCursorCycle reports a repeated tools/list pagination cursor.
	ErrCursorCycle = errors.New("remote MCP tools/list cursor cycle detected")
)

// Config is the authorized remote MCP endpoint configuration. BearerToken is
// never logged or included in errors.
type Config struct {
	URL string
	// BearerToken is sent as "Authorization: Bearer <token>". It is a secret
	// and must come from an ignored local config.
	BearerToken string
	// Timeout bounds one HTTP request. Zero uses limits.MCPRequestTimeout;
	// the value is clamped to (0, limits.UpstreamOpMax].
	Timeout time.Duration
	// HTTPClient optionally supplies a client (for example a TLS test
	// client). When nil, the default transport is used, which always
	// verifies server certificates.
	HTTPClient *http.Client
	// AllowTools is an optional explicit read-only allowlist. When empty,
	// only tools annotated readOnlyHint=true may be called. Tools annotated
	// readOnlyHint=false are always refused.
	AllowTools []string
}

// ServerInfo is the parsed initialize response.
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Capabilities    json.RawMessage
	Instructions    string
}

// Tool is one discovered remote tool.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Annotations json.RawMessage
}

// Content is one MCP tool result content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallResult is a read-only tool response.
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

// Error renders the error without the potentially large data field.
func (e *RPCError) Error() string {
	return fmt.Sprintf("remote MCP JSON-RPC error %d: %s", e.Code, e.Message)
}

// Unwrap allows errors.Is(err, ErrRPC).
func (e *RPCError) Unwrap() error { return ErrRPC }

// Client is a remote MCP Streamable HTTP client. It is safe for concurrent
// use.
type Client struct {
	url     string
	token   string
	timeout time.Duration
	http    *http.Client
	allow   map[string]bool

	mu              sync.Mutex
	nextID          int64
	sessionID       string
	protocolVersion string
	initialized     bool
	initializing    bool
	closed          bool
	tools           map[string]Tool
}

// New validates the configuration and returns a remote MCP client. The URL
// must be absolute HTTPS; the timeout must be bounded; no insecure TLS option
// is ever added.
func New(cfg Config) (*Client, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: URL is required", ErrConfig)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse URL: %v", ErrConfig, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: URL %q must use https (no plaintext fallback)", ErrConfig, rawURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: URL %q has no host", ErrConfig, rawURL)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: URL %q must not embed credentials; use BearerToken", ErrConfig, rawURL)
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: URL %q must not contain a fragment", ErrConfig, rawURL)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = limits.MCPRequestTimeout
	}
	if timeout < time.Millisecond || timeout > limits.UpstreamOpMax {
		return nil, fmt.Errorf("%w: timeout %s must be within (0, %s]", ErrConfig, timeout, limits.UpstreamOpMax)
	}

	var allow map[string]bool
	for _, name := range cfg.AllowTools {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%w: AllowTools contains an empty entry", ErrConfig)
		}
		if allow == nil {
			allow = make(map[string]bool)
		}
		allow[name] = true
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// The default transport verifies certificates; InsecureSkipVerify is
		// structurally absent from this package.
		httpClient = &http.Client{Timeout: timeout}
	} else if tr, ok := httpClient.Transport.(*http.Transport); ok && tr != nil &&
		tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		return nil, fmt.Errorf("%w: supplied HTTPClient disables TLS verification", ErrConfig)
	}

	return &Client{
		url:     parsed.String(),
		token:   cfg.BearerToken,
		timeout: timeout,
		http:    httpClient,
		allow:   allow,
		nextID:  1,
		tools:   make(map[string]Tool),
	}, nil
}

// SessionID returns the MCP session id negotiated during initialize, if any.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Close marks the client closed; subsequent requests fail with ErrClosed.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

// Initialize performs MCP initialization and sends the initialized
// notification once.
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return ServerInfo{}, ErrClosed
	case c.initialized || c.initializing:
		c.mu.Unlock()
		return ServerInfo{}, ErrAlreadyInitialized
	}
	c.initializing = true
	c.mu.Unlock()

	fail := func(err error) (ServerInfo, error) {
		c.mu.Lock()
		c.initializing = false
		c.mu.Unlock()
		return ServerInfo{}, err
	}

	params := map[string]any{
		"protocolVersion": ClientProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": ClientName, "version": ClientVersion},
	}
	raw, header, err := c.do(ctx, methodInitialize, params, true)
	if err != nil {
		return fail(err)
	}
	info, err := parseInitializeResult(raw)
	if err != nil {
		return fail(err)
	}

	c.mu.Lock()
	if session := header.Get(headerSessionID); session != "" {
		c.sessionID = session
	}
	c.protocolVersion = info.ProtocolVersion
	c.mu.Unlock()

	if err := c.sendNotification(ctx, methodInitialized, nil); err != nil {
		return fail(err)
	}

	c.mu.Lock()
	c.initialized = true
	c.initializing = false
	c.mu.Unlock()
	return info, nil
}

// ListTools discovers tools with bounded cursor pagination. The discovered set
// is stored so CallReadOnlyTool can refuse unknown tools locally.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.requireInitialized(); err != nil {
		return nil, err
	}
	var tools []Tool
	seenNames := make(map[string]bool)
	seenCursors := make(map[string]bool)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= limits.MCPMaxPages {
			return nil, fmt.Errorf("%w: more than %d pages", ErrTooManyPages, limits.MCPMaxPages)
		}
		var params any = map[string]any{}
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, _, err := c.do(ctx, methodListTools, params, true)
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
				continue // unsupported tool shape is not exposed
			}
			if seenNames[tool.Name] {
				continue
			}
			seenNames[tool.Name] = true
			tools = append(tools, tool)
			if len(tools) > limits.MCPMaxTools {
				return nil, fmt.Errorf("%w: more than %d tools", ErrTooManyTools, limits.MCPMaxTools)
			}
		}
		if nextCursor == "" {
			break
		}
		if seenCursors[nextCursor] {
			return nil, fmt.Errorf("%w: cursor %q repeated", ErrCursorCycle, nextCursor)
		}
		seenCursors[nextCursor] = true
		cursor = nextCursor
	}

	c.mu.Lock()
	c.tools = make(map[string]Tool, len(tools))
	for _, tool := range tools {
		c.tools[tool.Name] = tool
	}
	c.mu.Unlock()
	return tools, nil
}

// CallReadOnlyTool calls one discovered read-only tool. The permission check
// happens locally: a write tool, an undiscovered tool or a non-allowlisted
// tool returns an error without any HTTP request being sent.
func (c *Client) CallReadOnlyTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if err := c.requireInitialized(); err != nil {
		return CallResult{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return CallResult{}, fmt.Errorf("%w: tool name is empty", ErrInvalidRequest)
	}
	if len(args) > 0 && !isJSONObject(args) {
		return CallResult{}, fmt.Errorf("%w: tool arguments must be a JSON object", ErrInvalidRequest)
	}

	c.mu.Lock()
	tool, discovered := c.tools[name]
	allow := c.allow
	c.mu.Unlock()

	if !discovered {
		return CallResult{}, fmt.Errorf("%w: %q", ErrToolUnknown, name)
	}
	if allow != nil && !allow[name] {
		return CallResult{}, fmt.Errorf("%w: %q is not in AllowTools", ErrToolNotAllowed, name)
	}
	if readOnly, known := readOnlyHint(tool); known && !readOnly {
		return CallResult{}, fmt.Errorf("%w: %q declares readOnlyHint=false", ErrToolNotAllowed, name)
	} else if !known && allow == nil {
		return CallResult{}, fmt.Errorf("%w: %q has no readOnlyHint annotation and no allowlist entry", ErrToolNotAllowed, name)
	}

	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Name: name, Arguments: args}
	raw, _, err := c.do(ctx, methodCallTool, params, true)
	if err != nil {
		return CallResult{}, err
	}
	if !isJSONObject(raw) {
		return CallResult{}, fmt.Errorf("%w: tools/call result is not a JSON object", ErrProtocol)
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallResult{}, fmt.Errorf("%w: tools/call result: %v", ErrProtocol, err)
	}
	res.Raw = append(json.RawMessage(nil), raw...)
	if res.IsError {
		return res, fmt.Errorf("%w: %s", ErrToolError, res.Text())
	}
	return res, nil
}

func (c *Client) requireInitialized() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if !c.initialized {
		return ErrNotInitialized
	}
	return nil
}

func readOnlyHint(tool Tool) (readOnly bool, known bool) {
	if len(tool.Annotations) == 0 || !isJSONObject(tool.Annotations) {
		return false, false
	}
	var annotations struct {
		ReadOnlyHint *bool `json:"readOnlyHint"`
	}
	if err := json.Unmarshal(tool.Annotations, &annotations); err != nil || annotations.ReadOnlyHint == nil {
		return false, false
	}
	return *annotations.ReadOnlyHint, true
}

func parseInitializeResult(raw json.RawMessage) (ServerInfo, error) {
	if !isJSONObject(raw) {
		return ServerInfo{}, fmt.Errorf("%w: initialize result is not a JSON object", ErrProtocol)
	}
	var parsed struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ServerInfo{}, fmt.Errorf("%w: initialize result: %v", ErrProtocol, err)
	}
	if strings.TrimSpace(parsed.ProtocolVersion) == "" {
		return ServerInfo{}, fmt.Errorf("%w: initialize result has no protocolVersion", ErrProtocol)
	}
	if len(parsed.ServerInfo.Name) > 256 || len(parsed.ServerInfo.Version) > 256 {
		return ServerInfo{}, fmt.Errorf("%w: initialize serverInfo too long", ErrProtocol)
	}
	return ServerInfo{
		Name:            parsed.ServerInfo.Name,
		Version:         parsed.ServerInfo.Version,
		ProtocolVersion: parsed.ProtocolVersion,
		Capabilities:    append(json.RawMessage(nil), parsed.Capabilities...),
		Instructions:    parsed.Instructions,
	}, nil
}

func parseToolsPage(raw json.RawMessage) ([]json.RawMessage, string, error) {
	if !isJSONObject(raw) {
		return nil, "", fmt.Errorf("%w: tools/list result is not a JSON object", ErrProtocol)
	}
	var page struct {
		Tools      json.RawMessage `json:"tools"`
		NextCursor string          `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("%w: tools/list result: %v", ErrProtocol, err)
	}
	if len(page.Tools) == 0 || !isJSONArray(page.Tools) {
		return nil, "", fmt.Errorf("%w: tools/list result has no tools array", ErrProtocol)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(page.Tools, &tools); err != nil {
		return nil, "", fmt.Errorf("%w: tools/list tools: %v", ErrProtocol, err)
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
		Annotations json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Tool{}, false
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" || len(name) > 128 || strings.ContainsAny(name, " \t\r\n") {
		return Tool{}, false
	}
	if len(parsed.Description) > 4096 {
		return Tool{}, false
	}
	if !isJSONObject(parsed.InputSchema) {
		return Tool{}, false
	}
	return Tool{
		Name:        name,
		Description: parsed.Description,
		InputSchema: append(json.RawMessage(nil), parsed.InputSchema...),
		Annotations: append(json.RawMessage(nil), parsed.Annotations...),
	}, true
}

func isJSONObject(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return false
	}
	return json.Valid(raw)
}

func isJSONArray(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return false
	}
	return json.Valid(raw)
}

func mediaTypeOf(header http.Header) string {
	raw := header.Get("Content-Type")
	if raw == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return ""
	}
	return mediaType
}
