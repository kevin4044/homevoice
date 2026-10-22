// Package agent implements the bounded model tool loop for the M0 probe
// (docs/milestones/M0-transport-probe, sections R10 and T5).
//
// The model is only ever shown one synthetic tool,
// set_current_device_volume(level). The discovered device tool, its parameter
// name and its integer range come from the device's own schema and are bound
// before the turn starts. The model can never choose a target device: the
// executor maps the synthetic call onto the session-bound device tool.
//
// Every turn is bounded: at most limits.ModelMaxRequests model requests and
// limits.ModelMaxToolCalls tool executions. Additional requests or tool calls
// are rejected as bounded errors. Tool arguments are validated locally
// (integer type, schema range, no extra fields such as device_id) before any
// executor runs. A turn only counts as executed after a tool result was
// confirmed, and JSON-RPC/transport failures, result.isError, cancellation and
// timeouts are never converted into success.
//
// This package deliberately contains no memory, knowledge base, Skills
// platform, arbitrary tool registry or code execution.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// SyntheticToolName is the only tool exposed to the model.
const SyntheticToolName = "set_current_device_volume"

// DefaultModel is the placeholder model id sent with each request.
const DefaultModel = "m0-probe"

// Default bounds taken from the approved M0 specification.
const (
	DefaultMaxRequests  = limits.ModelMaxRequests
	DefaultMaxToolCalls = limits.ModelMaxToolCalls

	// DefaultRequestTimeout bounds one provider or executor call inside a
	// turn, independent of the caller's context.
	DefaultRequestTimeout = limits.UpstreamOpMax
)

// Errors returned by the loop. Callers distinguish failure classes with
// errors.Is.
var (
	// ErrNotBound reports a turn started without a bound device tool.
	ErrNotBound = errors.New("agent: no device tool bound; refusing to expose tools")

	// ErrUnsupportedSchema reports a discovered device schema that cannot be
	// mapped to the synthetic volume tool. An unsupported tool is not exposed.
	ErrUnsupportedSchema = errors.New("agent: discovered device schema is not a supported volume tool")

	// ErrInvalidToolCall reports a tool call name the model may not use.
	ErrInvalidToolCall = errors.New("agent: invalid tool call name")

	// ErrInvalidArguments reports a tool argument with a wrong type, an
	// out-of-range value, or extra fields.
	ErrInvalidArguments = errors.New("agent: invalid tool arguments")

	// ErrRequestLimit reports that the turn would exceed limits.ModelMaxRequests.
	ErrRequestLimit = errors.New("agent: model request limit exceeded")

	// ErrToolCallLimit reports that the turn would exceed
	// limits.ModelMaxToolCalls.
	ErrToolCallLimit = errors.New("agent: tool call limit exceeded")

	// ErrToolFailed reports an executor error or an isError tool result.
	ErrToolFailed = errors.New("agent: tool execution failed")

	// ErrUnconfirmed reports a model answer without a confirmed tool result.
	ErrUnconfirmed = errors.New("agent: no confirmed tool result")

	// ErrProvider reports a model provider failure.
	ErrProvider = errors.New("agent: model provider failed")

	// ErrCanceled reports a turn canceled or timed out by the caller or by
	// the per-call timeout bound.
	ErrCanceled = errors.New("agent: turn canceled or timed out")

	// ErrInvalidConfig reports nil dependencies or an unusable loop setup.
	ErrInvalidConfig = errors.New("agent: invalid loop configuration")
)

// ToolCall is one model-requested tool call.
type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult is the confirmed result of one tool execution.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Message is one conversation message. ToolCallID is a probe extension that
// correlates a tool result message with the assistant tool call; the model
// never supplies it.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Request is one bounded model request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []json.RawMessage
}

// Response is one model response.
type Response struct {
	Content   string
	ToolCalls []ToolCall
}

// ModelProvider is the single model API used by the probe. Implementations
// must honor ctx cancellation and return promptly.
type ModelProvider interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// ToolExecutor executes one validated synthetic tool call. Implementations map
// the call onto the authenticated session's device tool; the target device is
// never taken from the model.
type ToolExecutor interface {
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

// RunResult reports what one turn actually did. Content is model-produced text
// and is not by itself evidence of device execution; Executed is true only
// after a confirmed (non-error) tool result.
type RunResult struct {
	Content       string
	Executed      bool
	ToolResults   []ToolResult
	Requests      int
	ToolCallsMade int
}

// ToolBinding is the session-bound mapping from the synthetic tool to the
// discovered device tool. Enum, when non-empty, is the exact discrete set of
// allowed values; Minimum/Maximum still provide the model-facing range.
type ToolBinding struct {
	DeviceTool string
	Parameter  string
	Minimum    int
	Maximum    int
	Enum       []int
}

// Loop is a reusable bounded model tool loop. It is safe to configure while no
// Run is in flight; concurrent Runs each use a snapshot of the configuration.
type Loop struct {
	mu             sync.Mutex
	maxRequests    int
	maxToolCalls   int
	model          string
	systemPrompt   string
	messages       []Message
	binding        *ToolBinding
	toolSpec       json.RawMessage
	requestTimeout time.Duration
}

// NewLoop creates a loop. maxRequests and maxToolCalls of <= 0 use the M0
// defaults; values above the specification limits are clamped so the probe
// cannot grow its turn budget. maxRequests is the number of provider requests
// per turn, maxToolCalls the number of device tool executions per turn.
func NewLoop(maxRequests, maxToolCalls int) *Loop {
	if maxRequests <= 0 {
		maxRequests = limits.ModelMaxRequests
	}
	if maxRequests > limits.ModelMaxRequests {
		maxRequests = limits.ModelMaxRequests
	}
	if maxToolCalls <= 0 {
		maxToolCalls = limits.ModelMaxToolCalls
	}
	if maxToolCalls > limits.ModelMaxToolCalls {
		maxToolCalls = limits.ModelMaxToolCalls
	}
	return &Loop{
		maxRequests:    maxRequests,
		maxToolCalls:   maxToolCalls,
		model:          DefaultModel,
		requestTimeout: limits.UpstreamOpMax,
	}
}

// Bounds returns the effective per-turn bounds.
func (l *Loop) Bounds() (maxRequests, maxToolCalls int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxRequests, l.maxToolCalls
}

// SetModel sets the model id sent with each request.
func (l *Loop) SetModel(model string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.TrimSpace(model) != "" {
		l.model = model
	}
}

// SetSystemPrompt prepends a system message to each turn.
func (l *Loop) SetSystemPrompt(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.systemPrompt = text
}

// SetMessages seeds the per-turn conversation (for example the user request).
// The supplied slice is copied.
func (l *Loop) SetMessages(messages []Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = cloneMessages(messages)
}

// SetRequestTimeout bounds each provider and executor call inside a turn.
// Values <= 0 restore limits.UpstreamOpMax.
func (l *Loop) SetRequestTimeout(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d <= 0 {
		d = limits.UpstreamOpMax
	}
	l.requestTimeout = d
}

// BindVolumeTool maps the discovered device tool schema onto the single
// synthetic tool. Schema mismatches are rejected (the tool is not exposed), as
// are non-integer / unbounded / unrequired volume parameters.
func (l *Loop) BindVolumeTool(deviceTool string, inputSchema json.RawMessage) (ToolBinding, error) {
	binding, err := deriveVolumeBinding(deviceTool, inputSchema)
	if err != nil {
		return ToolBinding{}, err
	}
	spec, err := buildSyntheticToolSpec(binding)
	if err != nil {
		return ToolBinding{}, err
	}
	l.mu.Lock()
	l.binding = &binding
	l.toolSpec = spec
	l.mu.Unlock()
	return binding, nil
}

// SyntheticTool returns the exact tool descriptor exposed to the model.
func (l *Loop) SyntheticTool() (json.RawMessage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.binding == nil {
		return nil, false
	}
	return append(json.RawMessage(nil), l.toolSpec...), true
}

// Run executes one bounded turn. It returns a partial RunResult together with
// a bounded error for every rejected or failed path; Executed is true only
// when a confirmed non-error tool result exists.
func (l *Loop) Run(ctx context.Context, provider ModelProvider, executor ToolExecutor) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if provider == nil || executor == nil {
		return RunResult{}, fmt.Errorf("%w: provider and executor are required", ErrInvalidConfig)
	}

	l.mu.Lock()
	if l.binding == nil || len(l.toolSpec) == 0 {
		l.mu.Unlock()
		return RunResult{}, ErrNotBound
	}
	binding := *l.binding
	toolSpec := append(json.RawMessage(nil), l.toolSpec...)
	maxRequests, maxToolCalls := l.maxRequests, l.maxToolCalls
	model := l.model
	systemPrompt := l.systemPrompt
	requestTimeout := l.requestTimeout
	messages := cloneMessages(l.messages)
	l.mu.Unlock()

	if strings.TrimSpace(systemPrompt) != "" {
		messages = append([]Message{{Role: "system", Content: systemPrompt}}, messages...)
	}

	result := RunResult{}
	toolResults := []ToolResult{}
	confirmed := false
	ret := func(err error) (RunResult, error) {
		result.ToolResults = toolResults
		return result, err
	}

	for {
		if result.Requests >= maxRequests {
			return ret(fmt.Errorf("%w: %d request(s) already used", ErrRequestLimit, maxRequests))
		}
		result.Requests++
		req := Request{
			Model:    model,
			Messages: cloneMessages(messages),
			Tools:    []json.RawMessage{toolSpec},
		}
		resp, err := callProvider(ctx, provider, req, requestTimeout)
		if err != nil {
			return ret(err)
		}

		if len(resp.ToolCalls) == 0 {
			result.Content = resp.Content
			if !confirmed {
				return ret(fmt.Errorf("%w: refusing to report success for model text alone", ErrUnconfirmed))
			}
			result.Executed = true
			return ret(nil)
		}

		// Validate the complete batch and the bound before executing anything,
		// so an over-limit or invalid call never causes a partial side effect.
		if result.ToolCallsMade+len(resp.ToolCalls) > maxToolCalls {
			return ret(fmt.Errorf("%w: %d tool call(s) requested, %d allowed with %d already used",
				ErrToolCallLimit, len(resp.ToolCalls), maxToolCalls, result.ToolCallsMade))
		}
		for _, call := range resp.ToolCalls {
			if _, err := validateToolCall(binding, call); err != nil {
				return ret(err)
			}
		}

		messages = append(messages, Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: append([]ToolCall(nil), resp.ToolCalls...),
		})
		for _, call := range resp.ToolCalls {
			tr, err := executeTool(ctx, executor, call, requestTimeout)
			if err != nil {
				return ret(err)
			}
			if tr.CallID == "" {
				tr.CallID = call.ID
			} else if call.ID != "" && tr.CallID != call.ID {
				return ret(fmt.Errorf("%w: result call id %q does not match call %q", ErrToolFailed, tr.CallID, call.ID))
			}
			result.ToolCallsMade++
			toolResults = append(toolResults, tr)
			if tr.IsError {
				return ret(fmt.Errorf("%w: %s", ErrToolFailed, tr.Content))
			}
			messages = append(messages, Message{
				Role:       "tool",
				Content:    tr.Content,
				ToolCallID: tr.CallID,
			})
			confirmed = true
		}
	}
}

func callProvider(ctx context.Context, provider ModelProvider, req Request, timeout time.Duration) (Response, error) {
	callCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	resp, err := provider.Chat(callCtx, req)
	if err != nil {
		if ctx.Err() != nil || callCtx.Err() != nil ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Response{}, fmt.Errorf("%w: %v", ErrCanceled, err)
		}
		return Response{}, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	// A response returned after cancellation or timeout is not a success.
	if ctx.Err() != nil || callCtx.Err() != nil {
		return Response{}, fmt.Errorf("%w: provider returned after cancellation or timeout", ErrCanceled)
	}
	return resp, nil
}

func executeTool(ctx context.Context, executor ToolExecutor, call ToolCall, timeout time.Duration) (ToolResult, error) {
	callCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	tr, err := executor.Execute(callCtx, call)
	if err != nil {
		if ctx.Err() != nil || callCtx.Err() != nil ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return tr, fmt.Errorf("%w: %v", ErrCanceled, err)
		}
		return tr, fmt.Errorf("%w: %v", ErrToolFailed, err)
	}
	if ctx.Err() != nil || callCtx.Err() != nil {
		return tr, fmt.Errorf("%w: tool returned after cancellation or timeout", ErrCanceled)
	}
	return tr, nil
}

// validateToolCall enforces the model-facing contract: exactly the one
// synthetic tool, an integer level within the discovered range, and no extra
// fields (so the model cannot smuggle a target device id).
func validateToolCall(binding ToolBinding, call ToolCall) (int, error) {
	if call.Name != SyntheticToolName {
		return 0, fmt.Errorf("%w: %q is not exposed; only %q is available and the target device comes from the authenticated session",
			ErrInvalidToolCall, call.Name, SyntheticToolName)
	}
	return parseLevel(binding, call.Arguments)
}

func parseLevel(binding ToolBinding, args json.RawMessage) (int, error) {
	if len(strings.TrimSpace(string(args))) == 0 {
		return 0, fmt.Errorf("%w: level is required", ErrInvalidArguments)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return 0, fmt.Errorf("%w: arguments must be a JSON object with only \"level\"", ErrInvalidArguments)
	}
	if len(fields) != 1 {
		for name := range fields {
			if name != "level" {
				return 0, fmt.Errorf("%w: unknown field %q; the model may not supply a target", ErrInvalidArguments, name)
			}
		}
		return 0, fmt.Errorf("%w: exactly one level field is required", ErrInvalidArguments)
	}
	raw, ok := fields["level"]
	if !ok {
		return 0, fmt.Errorf("%w: level is required", ErrInvalidArguments)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text[0] == '"' || text[0] == '{' || text[0] == '[' ||
		text == "true" || text == "false" || text == "null" {
		return 0, fmt.Errorf("%w: level must be an integer", ErrInvalidArguments)
	}
	level, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%w: level must be an integer, got %q", ErrInvalidArguments, text)
	}
	if level < binding.Minimum || level > binding.Maximum {
		return 0, fmt.Errorf("%w: level %d outside discovered range [%d, %d]",
			ErrInvalidArguments, level, binding.Minimum, binding.Maximum)
	}
	if len(binding.Enum) > 0 {
		allowed := false
		for _, v := range binding.Enum {
			if v == level {
				allowed = true
				break
			}
		}
		if !allowed {
			return 0, fmt.Errorf("%w: level %d is not in the discovered enum %v",
				ErrInvalidArguments, level, binding.Enum)
		}
	}
	return level, nil
}

// deriveVolumeBinding reads the discovered device tool schema and maps it onto
// the synthetic tool. Only an integer, bounded, required volume-like parameter
// is accepted; the tool is not exposed otherwise.
func deriveVolumeBinding(deviceTool string, inputSchema json.RawMessage) (ToolBinding, error) {
	deviceTool = strings.TrimSpace(deviceTool)
	if deviceTool == "" {
		return ToolBinding{}, fmt.Errorf("%w: device tool name is empty", ErrUnsupportedSchema)
	}
	if len(inputSchema) == 0 || !isJSONObject(inputSchema) {
		return ToolBinding{}, fmt.Errorf("%w: tool %q inputSchema is not a JSON object", ErrUnsupportedSchema, deviceTool)
	}
	var root struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(inputSchema, &root); err != nil {
		return ToolBinding{}, fmt.Errorf("%w: tool %q inputSchema: %v", ErrUnsupportedSchema, deviceTool, err)
	}
	if root.Type != "object" {
		return ToolBinding{}, fmt.Errorf("%w: tool %q inputSchema type %q is not object", ErrUnsupportedSchema, deviceTool, root.Type)
	}

	parameter := ""
	if _, ok := root.Properties["volume"]; ok {
		parameter = "volume"
	} else if len(root.Properties) == 1 {
		for name := range root.Properties {
			parameter = name
		}
	}
	if parameter == "" {
		return ToolBinding{}, fmt.Errorf("%w: tool %q exposes no unambiguous volume parameter", ErrUnsupportedSchema, deviceTool)
	}
	required := false
	for _, name := range root.Required {
		if name == parameter {
			required = true
			continue
		}
		// The synthetic executor only supplies the volume-like parameter. Any
		// other required property cannot be mapped, so the tool must not be
		// exposed rather than silently sending an incomplete argument set.
		return ToolBinding{}, fmt.Errorf("%w: tool %q has unsupported required parameter %q besides %q",
			ErrUnsupportedSchema, deviceTool, name, parameter)
	}
	if !required {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q is not required", ErrUnsupportedSchema, deviceTool, parameter)
	}

	var property struct {
		Type             string          `json:"type"`
		Minimum          json.RawMessage `json:"minimum"`
		Maximum          json.RawMessage `json:"maximum"`
		ExclusiveMinimum json.RawMessage `json:"exclusiveMinimum"`
		ExclusiveMaximum json.RawMessage `json:"exclusiveMaximum"`
		Enum             json.RawMessage `json:"enum"`
	}
	if err := json.Unmarshal(root.Properties[parameter], &property); err != nil || property.Type != "integer" {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q is not an integer", ErrUnsupportedSchema, deviceTool, parameter)
	}
	if len(property.ExclusiveMinimum) > 0 || len(property.ExclusiveMaximum) > 0 {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q uses exclusive bounds", ErrUnsupportedSchema, deviceTool, parameter)
	}
	minimum, err := parseSchemaInt(property.Minimum)
	if err != nil {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q minimum: %v", ErrUnsupportedSchema, deviceTool, parameter, err)
	}
	maximum, err := parseSchemaInt(property.Maximum)
	if err != nil {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q maximum: %v", ErrUnsupportedSchema, deviceTool, parameter, err)
	}
	if minimum > maximum {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q has minimum > maximum", ErrUnsupportedSchema, deviceTool, parameter)
	}
	enum, err := parseSchemaEnum(property.Enum)
	if err != nil {
		return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q enum: %v", ErrUnsupportedSchema, deviceTool, parameter, err)
	}
	for _, v := range enum {
		if v < minimum || v > maximum {
			return ToolBinding{}, fmt.Errorf("%w: tool %q parameter %q enum value %d is outside [%d, %d]",
				ErrUnsupportedSchema, deviceTool, parameter, v, minimum, maximum)
		}
	}
	return ToolBinding{DeviceTool: deviceTool, Parameter: parameter, Minimum: minimum, Maximum: maximum, Enum: enum}, nil
}

// parseSchemaEnum validates an optional integer enum array. An empty/missing
// enum means no discrete restriction; malformed or non-integer enums are
// rejected instead of being silently widened.
func parseSchemaEnum(raw json.RawMessage) ([]int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("not an array")
	}
	if len(values) == 0 {
		return nil, errors.New("empty enum")
	}
	out := make([]int, 0, len(values))
	seen := make(map[int]bool, len(values))
	for _, item := range values {
		value, err := parseSchemaInt(item)
		if err != nil {
			return nil, err
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out, nil
}

func parseSchemaInt(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing bound")
	}
	var number json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return 0, fmt.Errorf("not a number: %v", err)
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, fmt.Errorf("not an integer: %v", err)
	}
	return value, nil
}

func buildSyntheticToolSpec(binding ToolBinding) (json.RawMessage, error) {
	spec := map[string]any{
		"name":        SyntheticToolName,
		"description": "Set the volume level of the current authenticated device session.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": func() map[string]any {
				level := map[string]any{
					"type":    "integer",
					"minimum": binding.Minimum,
					"maximum": binding.Maximum,
				}
				if len(binding.Enum) > 0 {
					level["enum"] = binding.Enum
				}
				return map[string]any{"level": level}
			}(),
			"required":             []string{"level"},
			"additionalProperties": false,
		},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal synthetic tool: %v", ErrInvalidConfig, err)
	}
	return raw, nil
}

func cloneMessages(in []Message) []Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		}
	}
	return out
}

func isJSONObject(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}'
}
