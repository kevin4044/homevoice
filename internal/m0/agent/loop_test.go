package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/agent"
	"github.com/homevoice/homevoice/internal/m0/fixtures"
)

// fixtureVolumeTool extracts the synthetic demo tool from the M0 control
// fixtures. The probe is not allowed to guess real firmware tool names; tests
// derive the range from the fixture schema.
func fixtureVolumeTool(t *testing.T) (string, json.RawMessage) {
	t.Helper()
	suite, err := fixtures.LoadControlMessages()
	if err != nil {
		t.Fatalf("load control fixtures: %v", err)
	}
	body, err := suite.Body("mcp-list-response")
	if err != nil {
		t.Fatalf("mcp-list-response fixture: %v", err)
	}
	var wrapper struct {
		Payload struct {
			Result struct {
				Tools []struct {
					Name        string          `json:"name"`
					InputSchema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(wrapper.Payload.Result.Tools) == 0 {
		t.Fatal("fixture has no tools")
	}
	return wrapper.Payload.Result.Tools[0].Name, wrapper.Payload.Result.Tools[0].InputSchema
}

func newBoundLoop(t *testing.T, maxRequests, maxToolCalls int) (*agent.Loop, agent.ToolBinding) {
	t.Helper()
	loop := agent.NewLoop(maxRequests, maxToolCalls)
	loop.SetModel("mock-model")
	name, schema := fixtureVolumeTool(t)
	binding, err := loop.BindVolumeTool(name, schema)
	if err != nil {
		t.Fatalf("BindVolumeTool: %v", err)
	}
	loop.SetMessages([]agent.Message{{Role: "user", Content: "请把当前设备音量调到 20"}})
	return loop, binding
}

// providerStep scripts one model response.
type providerStep struct {
	response agent.Response
	err      error
	block    bool
	delay    time.Duration
}

type scriptedProvider struct {
	mu     sync.Mutex
	steps  []providerStep
	calls  []agent.Request
	notify chan struct{}
}

func newScriptedProvider(steps ...providerStep) *scriptedProvider {
	return &scriptedProvider{steps: steps, notify: make(chan struct{}, 16)}
}

func (p *scriptedProvider) Chat(ctx context.Context, req agent.Request) (agent.Response, error) {
	cp := req
	cp.Messages = append([]agent.Message(nil), req.Messages...)
	cp.Tools = append([]json.RawMessage(nil), req.Tools...)
	p.mu.Lock()
	idx := len(p.calls)
	p.calls = append(p.calls, cp)
	step := providerStep{err: errors.New("scripted provider exhausted")}
	if idx < len(p.steps) {
		step = p.steps[idx]
	}
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
	if step.block {
		<-ctx.Done()
		return agent.Response{}, ctx.Err()
	}
	if step.delay > 0 {
		select {
		case <-time.After(step.delay):
		case <-ctx.Done():
			return agent.Response{}, ctx.Err()
		}
	}
	return step.response, step.err
}

func (p *scriptedProvider) requests() []agent.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.Request(nil), p.calls...)
}

func (p *scriptedProvider) waitCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		got := len(p.calls)
		p.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d provider calls, have %d", n, got)
		}
		select {
		case <-p.notify:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func callStep(id string, level int) providerStep {
	args, _ := json.Marshal(map[string]int{"level": level})
	return providerStep{response: agent.Response{ToolCalls: []agent.ToolCall{{
		ID:        id,
		Name:      agent.SyntheticToolName,
		Arguments: args,
	}}}}
}

func rawCallStep(id, name, args string) providerStep {
	return providerStep{response: agent.Response{ToolCalls: []agent.ToolCall{{
		ID:        id,
		Name:      name,
		Arguments: json.RawMessage(args),
	}}}}
}

func textStep(text string) providerStep {
	return providerStep{response: agent.Response{Content: text}}
}

// recordingExecutor records tool calls and returns scripted or default results.
type recordingExecutor struct {
	mu      sync.Mutex
	calls   []agent.ToolCall
	respond func(call agent.ToolCall, index int) (agent.ToolResult, error)
}

func (e *recordingExecutor) Execute(_ context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	e.mu.Lock()
	idx := len(e.calls)
	e.calls = append(e.calls, call)
	respond := e.respond
	e.mu.Unlock()
	if respond != nil {
		return respond(call, idx)
	}
	return agent.ToolResult{CallID: call.ID, Content: "ok"}, nil
}

func (e *recordingExecutor) executed() []agent.ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]agent.ToolCall(nil), e.calls...)
}

// deviceCall is one call recorded by recordingDeviceCaller.
type deviceCall struct {
	name string
	args json.RawMessage
}

type recordingDeviceCaller struct {
	mu     sync.Mutex
	calls  []deviceCall
	result agent.DeviceCallResult
	err    error
}

func (c *recordingDeviceCaller) CallDeviceTool(_ context.Context, name string, args json.RawMessage) (agent.DeviceCallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, deviceCall{name: name, args: append(json.RawMessage(nil), args...)})
	return c.result, c.err
}

func (c *recordingDeviceCaller) recorded() []deviceCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]deviceCall(nil), c.calls...)
}

// checkSyntheticOnly asserts the model saw exactly the synthetic tool and
// never the discovered device tool name.
func checkSyntheticOnly(t *testing.T, req agent.Request) {
	t.Helper()
	if len(req.Tools) != 1 {
		t.Fatalf("model request exposes %d tools, want exactly 1", len(req.Tools))
	}
	var tool struct {
		Name        string `json:"name"`
		InputSchema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type    string `json:"type"`
				Minimum int    `json:"minimum"`
				Maximum int    `json:"maximum"`
			} `json:"properties"`
			Required             []string `json:"required"`
			AdditionalProperties bool     `json:"additionalProperties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(req.Tools[0], &tool); err != nil {
		t.Fatalf("tool descriptor is not JSON: %v", err)
	}
	if tool.Name != agent.SyntheticToolName {
		t.Fatalf("model tool name = %q, want %q", tool.Name, agent.SyntheticToolName)
	}
	if strings.Contains(string(req.Tools[0]), "demo.speaker.set_volume") {
		t.Fatalf("model request leaked the discovered device tool name: %s", req.Tools[0])
	}
	level, ok := tool.InputSchema.Properties["level"]
	if !ok || level.Type != "integer" || level.Minimum != 0 || level.Maximum != 30 {
		t.Fatalf("level schema = %+v (want integer 0..30 from the fixture)", level)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "level" {
		t.Fatalf("required = %v, want [level]", tool.InputSchema.Required)
	}
	if tool.InputSchema.AdditionalProperties {
		t.Fatalf("additionalProperties must be false")
	}
}

// ---------------------------------------------------------------------------
// A24: only one session-bound synthetic tool, strict argument validation.
// ---------------------------------------------------------------------------

func TestA24OnlySyntheticSessionBoundTool(t *testing.T) {
	loop, binding := newBoundLoop(t, 0, 0)
	if binding.DeviceTool != "demo.speaker.set_volume" {
		t.Fatalf("binding device tool = %q", binding.DeviceTool)
	}
	if binding.Parameter != "volume" || binding.Minimum != 0 || binding.Maximum != 30 {
		t.Fatalf("binding = %+v, want volume 0..30", binding)
	}

	executor := &recordingExecutor{}
	provider := newScriptedProvider(
		callStep("call-1", 20),
		textStep("已把当前设备音量设为 20"),
	)
	res, err := loop.Run(context.Background(), provider, executor)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Executed {
		t.Fatalf("Executed = false after a confirmed tool result: %+v", res)
	}
	if res.Content != "已把当前设备音量设为 20" {
		t.Fatalf("Content = %q", res.Content)
	}
	if res.Requests != 2 || res.ToolCallsMade != 1 {
		t.Fatalf("requests/toolcalls = %d/%d, want 2/1", res.Requests, res.ToolCallsMade)
	}
	if len(res.ToolResults) != 1 || res.ToolResults[0].CallID != "call-1" || res.ToolResults[0].Content != "ok" {
		t.Fatalf("tool results = %+v", res.ToolResults)
	}

	calls := executor.executed()
	if len(calls) != 1 {
		t.Fatalf("executor ran %d calls, want 1", len(calls))
	}
	if calls[0].Name != agent.SyntheticToolName {
		t.Fatalf("executor call name = %q, want synthetic tool", calls[0].Name)
	}
	// The model supplied only level; the target comes from the binding.
	var supplied map[string]json.RawMessage
	if err := json.Unmarshal(calls[0].Arguments, &supplied); err != nil {
		t.Fatalf("executor arguments: %v", err)
	}
	if len(supplied) != 1 {
		t.Fatalf("executor arguments = %s, want exactly level", calls[0].Arguments)
	}
	if string(supplied["level"]) != "20" {
		t.Fatalf("executor level = %s", supplied["level"])
	}

	reqs := provider.requests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(reqs))
	}
	for _, req := range reqs {
		checkSyntheticOnly(t, req)
		if req.Model != "mock-model" {
			t.Fatalf("request model = %q", req.Model)
		}
	}
	if len(reqs[0].Messages) == 0 || reqs[0].Messages[0].Role != "user" {
		t.Fatalf("first request does not include the seeded user message: %+v", reqs[0].Messages)
	}
	// The second request carries the confirmed tool result, not just model text.
	foundToolResult := false
	for _, msg := range reqs[1].Messages {
		if msg.Role == "tool" && msg.ToolCallID == "call-1" && msg.Content == "ok" {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Fatalf("second request lacks the confirmed tool result: %+v", reqs[1].Messages)
	}
}

func TestA24RejectsInvalidArgumentsAndForeignTools(t *testing.T) {
	cases := []struct {
		name string
		call agent.ToolCall
		want error
	}{
		{
			name: "extra device_id",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20,"device_id":"dev-b"}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "extra target field",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20,"target":"dev-b"}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "string level",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":"20"}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "float level",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20.5}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "bool level",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":true}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "above discovered maximum",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":31}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "below discovered minimum",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":-1}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "missing level",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{}`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "array arguments",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`[20]`)},
			want: agent.ErrInvalidArguments,
		},
		{
			name: "device tool name",
			call: agent.ToolCall{ID: "c1", Name: "demo.speaker.set_volume", Arguments: json.RawMessage(`{"volume":20}`)},
			want: agent.ErrInvalidToolCall,
		},
		{
			name: "device parameter instead of level",
			call: agent.ToolCall{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"volume":20}`)},
			want: agent.ErrInvalidArguments,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, _ := newBoundLoop(t, 0, 0)
			executor := &recordingExecutor{}
			provider := newScriptedProvider(providerStep{response: agent.Response{ToolCalls: []agent.ToolCall{tc.call}}})
			res, err := loop.Run(context.Background(), provider, executor)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			if res.Executed {
				t.Fatalf("invalid call was reported as executed: %+v", res)
			}
			if len(executor.executed()) != 0 {
				t.Fatalf("executor ran for a rejected call: %+v", executor.executed())
			}
			if got := len(provider.requests()); got != 1 {
				t.Fatalf("provider requests = %d, want 1", got)
			}
		})
	}
}

func TestA24SchemaMismatchNotExposed(t *testing.T) {
	cases := []struct {
		name       string
		deviceTool string
		schema     string
	}{
		{
			name:       "string volume type",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"type":"object","properties":{"volume":{"type":"string"}},"required":["volume"]}`,
		},
		{
			name:       "missing bounds",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"type":"object","properties":{"volume":{"type":"integer"}},"required":["volume"]}`,
		},
		{
			name:       "volume not required",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30}}}`,
		},
		{
			name:       "minimum greater than maximum",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"type":"object","properties":{"volume":{"type":"integer","minimum":30,"maximum":0}},"required":["volume"]}`,
		},
		{
			name:       "exclusive bounds",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"type":"object","properties":{"volume":{"type":"integer","exclusiveMinimum":0,"maximum":30}},"required":["volume"]}`,
		},
		{
			name:       "no unambiguous volume parameter",
			deviceTool: "demo.device.control",
			schema:     `{"type":"object","properties":{"brightness":{"type":"integer","minimum":0,"maximum":100},"speed":{"type":"integer","minimum":1,"maximum":3}},"required":["brightness","speed"]}`,
		},
		{
			name:       "non-object schema",
			deviceTool: "demo.speaker.set_volume",
			schema:     `["not","an","object"]`,
		},
		{
			name:       "missing root type",
			deviceTool: "demo.speaker.set_volume",
			schema:     `{"properties":{"volume":{"type":"integer","minimum":0,"maximum":30}},"required":["volume"]}`,
		},
		{
			name:       "empty device tool name",
			deviceTool: "",
			schema:     `{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30}},"required":["volume"]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := agent.NewLoop(0, 0)
			_, err := loop.BindVolumeTool(tc.deviceTool, json.RawMessage(tc.schema))
			if !errors.Is(err, agent.ErrUnsupportedSchema) {
				t.Fatalf("BindVolumeTool error = %v, want ErrUnsupportedSchema", err)
			}
			provider := newScriptedProvider(textStep("should never run"))
			executor := &recordingExecutor{}
			res, err := loop.Run(context.Background(), provider, executor)
			if !errors.Is(err, agent.ErrNotBound) {
				t.Fatalf("Run error = %v, want ErrNotBound", err)
			}
			if res.Executed || len(provider.requests()) != 0 || len(executor.executed()) != 0 {
				t.Fatalf("unsupported schema was exposed: res=%+v provider=%d executor=%d",
					res, len(provider.requests()), len(executor.executed()))
			}
		})
	}
}

func TestA24SingleIntegerPropertyFallback(t *testing.T) {
	loop := agent.NewLoop(0, 0)
	binding, err := loop.BindVolumeTool("demo.device.volume",
		json.RawMessage(`{"type":"object","properties":{"level":{"type":"integer","minimum":1,"maximum":5}},"required":["level"]}`))
	if err != nil {
		t.Fatalf("BindVolumeTool: %v", err)
	}
	if binding.Parameter != "level" || binding.Minimum != 1 || binding.Maximum != 5 {
		t.Fatalf("binding = %+v", binding)
	}
}

func TestA24VolumeExecutorMapsSessionBoundDeviceTool(t *testing.T) {
	_, binding := newBoundLoop(t, 0, 0)
	caller := &recordingDeviceCaller{result: agent.DeviceCallResult{Content: "音量已设置"}}
	executor := agent.NewVolumeExecutor(binding, caller)

	res, err := executor.Execute(context.Background(), agent.ToolCall{
		ID:        "call-1",
		Name:      agent.SyntheticToolName,
		Arguments: json.RawMessage(`{"level":20}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.CallID != "call-1" || res.IsError || res.Content != "音量已设置" {
		t.Fatalf("result = %+v", res)
	}
	calls := caller.recorded()
	if len(calls) != 1 {
		t.Fatalf("device calls = %d, want 1", len(calls))
	}
	if calls[0].name != "demo.speaker.set_volume" {
		t.Fatalf("device tool = %q, want demo.speaker.set_volume", calls[0].name)
	}
	if len(calls[0].args) == 0 {
		t.Fatal("device call has no arguments")
	}
	var deviceArgs map[string]json.RawMessage
	if err := json.Unmarshal(calls[0].args, &deviceArgs); err != nil {
		t.Fatalf("device args: %v", err)
	}
	if string(deviceArgs["volume"]) != "20" || len(deviceArgs) != 1 {
		t.Fatalf("device args = %s, want only volume=20", calls[0].args)
	}

	// A foreign tool name is refused locally before reaching the device.
	if _, err := executor.Execute(context.Background(), agent.ToolCall{
		ID: "c2", Name: "demo.speaker.set_volume", Arguments: json.RawMessage(`{"volume":20}`),
	}); !errors.Is(err, agent.ErrInvalidToolCall) {
		t.Fatalf("foreign tool error = %v, want ErrInvalidToolCall", err)
	}
	// Out-of-range values are refused locally too.
	if _, err := executor.Execute(context.Background(), agent.ToolCall{
		ID: "c3", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":99}`),
	}); !errors.Is(err, agent.ErrInvalidArguments) {
		t.Fatalf("out of range error = %v, want ErrInvalidArguments", err)
	}
	// Extra device_id is refused locally.
	if _, err := executor.Execute(context.Background(), agent.ToolCall{
		ID: "c4", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20,"device_id":"dev-b"}`),
	}); !errors.Is(err, agent.ErrInvalidArguments) {
		t.Fatalf("extra device_id error = %v, want ErrInvalidArguments", err)
	}
	if len(caller.recorded()) != 1 {
		t.Fatalf("device was called for a rejected request: %+v", caller.recorded())
	}

	// isError results pass through and are never success.
	caller.result = agent.DeviceCallResult{Content: "设备拒绝", IsError: true}
	res, err = executor.Execute(context.Background(), agent.ToolCall{
		ID: "c5", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20}`),
	})
	if err != nil {
		t.Fatalf("Execute isError result should not be a transport error: %v", err)
	}
	if !res.IsError || res.Content != "设备拒绝" || res.CallID != "c5" {
		t.Fatalf("isError result = %+v", res)
	}

	// Transport errors are surfaced as failures.
	caller.err = errors.New("device MCP transport failed")
	if _, err := executor.Execute(context.Background(), agent.ToolCall{
		ID: "c6", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20}`),
	}); !errors.Is(err, agent.ErrToolFailed) {
		t.Fatalf("transport error = %v, want ErrToolFailed", err)
	}
}

// ---------------------------------------------------------------------------
// A25: hard bounds, timeouts, cancellation, no unconfirmed success.
// ---------------------------------------------------------------------------

func TestA25DefaultsAreClamped(t *testing.T) {
	loop := agent.NewLoop(0, 0)
	requests, tools := loop.Bounds()
	if requests != 2 || tools != 1 {
		t.Fatalf("default bounds = %d/%d, want 2/1", requests, tools)
	}
	loop = agent.NewLoop(100, 100)
	requests, tools = loop.Bounds()
	if requests != 2 || tools != 1 {
		t.Fatalf("oversized bounds were not clamped: %d/%d", requests, tools)
	}
}

func TestA25ToolCallLimit(t *testing.T) {
	t.Run("batch rejected before execution", func(t *testing.T) {
		loop, _ := newBoundLoop(t, 0, 1)
		executor := &recordingExecutor{}
		provider := newScriptedProvider(providerStep{response: agent.Response{ToolCalls: []agent.ToolCall{
			{ID: "c1", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":10}`)},
			{ID: "c2", Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20}`)},
		}}})
		res, err := loop.Run(context.Background(), provider, executor)
		if !errors.Is(err, agent.ErrToolCallLimit) {
			t.Fatalf("Run error = %v, want ErrToolCallLimit", err)
		}
		if res.Executed || res.ToolCallsMade != 0 {
			t.Fatalf("over-limit batch was partially executed: %+v", res)
		}
		if len(executor.executed()) != 0 {
			t.Fatalf("executor ran despite over-limit batch: %+v", executor.executed())
		}
	})

	t.Run("second turn tool call rejected", func(t *testing.T) {
		loop, _ := newBoundLoop(t, 0, 1)
		executor := &recordingExecutor{}
		provider := newScriptedProvider(callStep("c1", 10), callStep("c2", 20))
		res, err := loop.Run(context.Background(), provider, executor)
		if !errors.Is(err, agent.ErrToolCallLimit) {
			t.Fatalf("Run error = %v, want ErrToolCallLimit", err)
		}
		if res.Executed {
			t.Fatalf("over-limit turn reported as executed: %+v", res)
		}
		if res.ToolCallsMade != 1 || len(executor.executed()) != 1 {
			t.Fatalf("executions = %d/%d, want exactly 1", res.ToolCallsMade, len(executor.executed()))
		}
		if got := len(provider.requests()); got != 2 {
			t.Fatalf("provider requests = %d, want 2", got)
		}
	})
}

func TestA25RequestLimit(t *testing.T) {
	loop, _ := newBoundLoop(t, 1, 1)
	executor := &recordingExecutor{}
	provider := newScriptedProvider(callStep("c1", 10))
	res, err := loop.Run(context.Background(), provider, executor)
	if !errors.Is(err, agent.ErrRequestLimit) {
		t.Fatalf("Run error = %v, want ErrRequestLimit", err)
	}
	if res.Executed {
		t.Fatalf("turn reported success after exceeding the request bound: %+v", res)
	}
	if res.ToolCallsMade != 1 || len(res.ToolResults) != 1 {
		t.Fatalf("confirmed tool result lost: %+v", res)
	}
	if got := len(provider.requests()); got != 1 {
		t.Fatalf("provider requests = %d, want exactly 1", got)
	}
}

func TestA25ProviderTimeoutNotSuccess(t *testing.T) {
	loop, _ := newBoundLoop(t, 0, 0)
	loop.SetRequestTimeout(25 * time.Millisecond)
	provider := newScriptedProvider(providerStep{block: true})
	executor := &recordingExecutor{}

	start := time.Now()
	res, err := loop.Run(context.Background(), provider, executor)
	elapsed := time.Since(start)
	if !errors.Is(err, agent.ErrCanceled) {
		t.Fatalf("Run error = %v, want ErrCanceled", err)
	}
	if res.Executed {
		t.Fatalf("timed-out provider response counted as success: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("provider timeout was not bounded: %s", elapsed)
	}
	if len(executor.executed()) != 0 {
		t.Fatalf("executor ran without a provider response")
	}
}

func TestA25CallerCancellationNotSuccess(t *testing.T) {
	loop, _ := newBoundLoop(t, 0, 0)
	provider := newScriptedProvider(providerStep{block: true})
	executor := &recordingExecutor{}
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		res agent.RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := loop.Run(ctx, provider, executor)
		done <- outcome{res: res, err: err}
	}()
	provider.waitCalls(t, 1)
	cancel()
	select {
	case out := <-done:
		if !errors.Is(out.err, agent.ErrCanceled) {
			t.Fatalf("Run error = %v, want ErrCanceled", out.err)
		}
		if out.res.Executed {
			t.Fatalf("canceled turn counted as success: %+v", out.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestA25ToolFailureNotSuccess(t *testing.T) {
	t.Run("isError result", func(t *testing.T) {
		loop, _ := newBoundLoop(t, 0, 0)
		executor := &recordingExecutor{respond: func(call agent.ToolCall, _ int) (agent.ToolResult, error) {
			return agent.ToolResult{CallID: call.ID, Content: "合成执行失败", IsError: true}, nil
		}}
		provider := newScriptedProvider(callStep("c1", 20), textStep("should not be requested"))
		res, err := loop.Run(context.Background(), provider, executor)
		if !errors.Is(err, agent.ErrToolFailed) {
			t.Fatalf("Run error = %v, want ErrToolFailed", err)
		}
		if res.Executed {
			t.Fatalf("isError result counted as success: %+v", res)
		}
		if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
			t.Fatalf("tool results = %+v", res.ToolResults)
		}
		if got := len(provider.requests()); got != 1 {
			t.Fatalf("provider requests = %d, want 1 (no follow-up after failure)", got)
		}
	})

	t.Run("executor transport error", func(t *testing.T) {
		loop, _ := newBoundLoop(t, 0, 0)
		executor := &recordingExecutor{respond: func(call agent.ToolCall, _ int) (agent.ToolResult, error) {
			return agent.ToolResult{CallID: call.ID}, errors.New("device offline")
		}}
		provider := newScriptedProvider(callStep("c1", 20))
		res, err := loop.Run(context.Background(), provider, executor)
		if !errors.Is(err, agent.ErrToolFailed) {
			t.Fatalf("Run error = %v, want ErrToolFailed", err)
		}
		if res.Executed {
			t.Fatalf("failed executor counted as success: %+v", res)
		}
	})

	t.Run("call id mismatch", func(t *testing.T) {
		loop, _ := newBoundLoop(t, 0, 0)
		executor := &recordingExecutor{respond: func(_ agent.ToolCall, _ int) (agent.ToolResult, error) {
			return agent.ToolResult{CallID: "other-call", Content: "ok"}, nil
		}}
		provider := newScriptedProvider(callStep("c1", 20))
		res, err := loop.Run(context.Background(), provider, executor)
		if !errors.Is(err, agent.ErrToolFailed) {
			t.Fatalf("Run error = %v, want ErrToolFailed", err)
		}
		if res.Executed {
			t.Fatalf("mismatched result counted as success: %+v", res)
		}
	})
}

func TestA25ModelTextAloneIsNotSuccess(t *testing.T) {
	loop, _ := newBoundLoop(t, 0, 0)
	executor := &recordingExecutor{}
	provider := newScriptedProvider(textStep("已经把当前设备音量设为 20 了"))
	res, err := loop.Run(context.Background(), provider, executor)
	if !errors.Is(err, agent.ErrUnconfirmed) {
		t.Fatalf("Run error = %v, want ErrUnconfirmed", err)
	}
	if res.Executed {
		t.Fatalf("model text alone was treated as device success: %+v", res)
	}
	if res.Content != "已经把当前设备音量设为 20 了" {
		t.Fatalf("model text was not preserved for diagnostics: %q", res.Content)
	}
	if len(res.ToolResults) != 0 || len(executor.executed()) != 0 {
		t.Fatalf("tool was executed without a tool call")
	}
}

func TestA25ProviderErrorIsBoundedFailure(t *testing.T) {
	loop, _ := newBoundLoop(t, 0, 0)
	provider := newScriptedProvider(providerStep{err: errors.New("upstream 500")})
	res, err := loop.Run(context.Background(), provider, &recordingExecutor{})
	if !errors.Is(err, agent.ErrProvider) {
		t.Fatalf("Run error = %v, want ErrProvider", err)
	}
	if res.Executed {
		t.Fatalf("provider error counted as success: %+v", res)
	}
	if got := len(provider.requests()); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}
