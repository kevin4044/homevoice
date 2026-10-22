package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/homevoice/homevoice/internal/m0/fixtures"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

// MCPToolResult is the synthetic device tool result.
type MCPToolResult struct {
	Content string
	IsError bool
}

// MCPTool is one synthetic device MCP tool.
type MCPTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(args json.RawMessage) (MCPToolResult, error)
}

// MCPCall records one tools/call received by the simulated device.
type MCPCall struct {
	Name string
	Args json.RawMessage
	At   int64
}

// MCPOptions configures the synthetic device MCP server.
type MCPOptions struct {
	ProtocolVersion string
	ServerName      string
	ServerVersion   string
	PageSize        int
	// InitError makes initialize return a JSON-RPC error.
	InitError bool
	// CallError makes every tool call return result.isError=true.
	CallError bool
	// ToolsError makes tools/list return a JSON-RPC error.
	ToolsError bool
	// ToolNames overrides the tool set (tests for unsupported/extra tools).
	ToolNames []string
}

// MCPServer is a minimal synthetic device MCP server speaking JSON-RPC 2.0
// over the MQTT application wrapper. It is test infrastructure, not firmware.
type MCPServer struct {
	opts  MCPOptions
	tools []MCPTool
	pos   map[string]int

	mu        sync.Mutex
	calls     []MCPCall
	initCalls int
	listCalls int
}

// NewMCPServer creates a synthetic MCP server. PageSize defaults to 64 and is
// bounded by limits.MCPMaxTools.
func NewMCPServer(opts MCPOptions) *MCPServer {
	if opts.ProtocolVersion == "" {
		opts.ProtocolVersion = "2024-11-05"
	}
	if opts.ServerName == "" {
		opts.ServerName = "simulated-device"
	}
	if opts.ServerVersion == "" {
		opts.ServerVersion = "0.1.0"
	}
	if opts.PageSize <= 0 || opts.PageSize > limits.MCPMaxTools {
		opts.PageSize = 64
	}
	s := &MCPServer{opts: opts, pos: map[string]int{}}
	if len(opts.ToolNames) > 0 {
		for _, name := range opts.ToolNames {
			s.tools = append(s.tools, MCPTool{
				Name:        name,
				Description: "synthetic tool",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
				Handler: func(json.RawMessage) (MCPToolResult, error) {
					return MCPToolResult{Content: "ok"}, nil
				},
			})
		}
	} else {
		s.tools = []MCPTool{volumeTool()}
	}
	for i, t := range s.tools {
		s.pos[t.Name] = i
	}
	return s
}

// DefaultMCPServer returns the synthetic demo volume tool whose schema mirrors
// the public control-message fixture. The tool only records a call; it never
// writes real hardware state.
func DefaultMCPServer() *MCPServer { return NewMCPServer(MCPOptions{}) }

func volumeTool() MCPTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"volume": {"type": "integer", "minimum": 0, "maximum": 30}},
		"required": ["volume"],
		"additionalProperties": false
	}`)
	// Prefer the public fixture schema so the simulator cannot drift from the
	// documented synthetic input.
	if suite, err := fixtures.LoadControlMessages(); err == nil {
		if raw, err := suite.Body("mcp-list-response"); err == nil {
			var payload struct {
				Result struct {
					Tools []struct {
						Name        string          `json:"name"`
						Description string          `json:"description"`
						InputSchema json.RawMessage `json:"inputSchema"`
					} `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal(raw, &payload) == nil && len(payload.Result.Tools) > 0 &&
				payload.Result.Tools[0].InputSchema != nil {
				schema = payload.Result.Tools[0].InputSchema
			}
		}
	}
	var tool MCPTool
	tool.Name = "demo.speaker.set_volume"
	tool.Description = "合成测试工具（仅记录调用，不操作真实设备）"
	tool.InputSchema = schema
	recorded := int64(0)
	tool.Handler = func(args json.RawMessage) (MCPToolResult, error) {
		var in struct {
			Volume *int `json:"volume"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return MCPToolResult{IsError: true, Content: "invalid arguments"}, nil
		}
		if in.Volume == nil {
			return MCPToolResult{IsError: true, Content: "volume is required"}, nil
		}
		if *in.Volume < 0 || *in.Volume > 30 {
			return MCPToolResult{IsError: true, Content: "volume out of range"}, nil
		}
		_ = recorded
		return MCPToolResult{Content: fmt.Sprintf("synthetic volume recorded: %d", *in.Volume)}, nil
	}
	return tool
}

// Handle processes one JSON-RPC message payload. ok is false for notifications
// that require no response.
func (s *MCPServer) Handle(payload []byte) (resp json.RawMessage, ok bool) {
	if len(payload) > limits.MaxJSONBytes {
		return rpcError(nil, -32600, "payload too large", true)
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Method == "" {
		return rpcError(nil, -32600, "invalid request", true)
	}
	if req.JSONRPC != "2.0" {
		return rpcError(req.ID, -32600, "unsupported jsonrpc version", true)
	}
	switch req.Method {
	case "initialize":
		if s.opts.InitError {
			return rpcError(req.ID, -32603, "synthetic initialize failure", true)
		}
		s.mu.Lock()
		s.initCalls++
		s.mu.Unlock()
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": s.opts.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.opts.ServerName, "version": s.opts.ServerVersion},
		})
	case "notifications/initialized":
		return nil, false
	case "notifications/cancelled":
		return nil, false
	case "tools/list":
		if s.opts.ToolsError {
			return rpcError(req.ID, -32603, "synthetic tools/list failure", true)
		}
		s.mu.Lock()
		s.listCalls++
		s.mu.Unlock()
		start := 0
		if len(req.Params) > 0 {
			var p struct {
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return rpcError(req.ID, -32602, "invalid cursor", true)
			}
			if p.Cursor != "" {
				n, err := strconv.Atoi(strings.TrimSpace(p.Cursor))
				if err != nil || n < 0 || n > len(s.tools) {
					return rpcError(req.ID, -32602, "invalid cursor", true)
				}
				start = n
			}
		}
		end := start + s.opts.PageSize
		if end > len(s.tools) {
			end = len(s.tools)
		}
		page := make([]map[string]any, 0, end-start)
		for _, t := range s.tools[start:end] {
			page = append(page, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": json.RawMessage(t.InputSchema),
			})
		}
		result := map[string]any{"tools": page}
		if end < len(s.tools) {
			result["nextCursor"] = strconv.Itoa(end)
		}
		return rpcResult(req.ID, result)
	case "tools/call":
		if s.opts.CallError {
			return rpcResult(req.ID, map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "synthetic device failure"}},
			})
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcError(req.ID, -32602, "invalid tool call params", true)
		}
		idx, found := s.pos[p.Name]
		if !found {
			return rpcError(req.ID, -32601, "unknown tool", true)
		}
		args := p.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		res, err := s.tools[idx].Handler(args)
		if err != nil {
			return rpcError(req.ID, -32603, "tool handler failed", true)
		}
		s.mu.Lock()
		s.calls = append(s.calls, MCPCall{Name: p.Name, Args: append(json.RawMessage(nil), args...)})
		s.mu.Unlock()
		return rpcResult(req.ID, map[string]any{
			"isError": res.IsError,
			"content": []map[string]any{{"type": "text", "text": res.Content}},
		})
	default:
		return rpcError(req.ID, -32601, "method not found", true)
	}
}

// Calls returns recorded tool calls.
func (s *MCPServer) Calls() []MCPCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MCPCall(nil), s.calls...)
}

// Tools returns the synthetic tool list.
func (s *MCPServer) Tools() []MCPTool {
	return append([]MCPTool(nil), s.tools...)
}

// Counts returns initialize and tools/list counts for assertions.
func (s *MCPServer) Counts() (initialize, list int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initCalls, s.listCalls
}

func rpcResult(id json.RawMessage, result any) (json.RawMessage, bool) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return nil, false
	}
	return b, true
}

func rpcError(id json.RawMessage, code int, message string, ok bool) (json.RawMessage, bool) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return nil, false
	}
	return b, ok
}

// ErrMCPUnavailable is returned by helpers when the synthetic server rejects a
// request; it is not used to fake success.
var ErrMCPUnavailable = errors.New("simulator: synthetic device MCP unavailable")
