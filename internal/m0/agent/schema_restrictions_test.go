package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/agent"
)

type recordingCaller struct {
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	name string
	args json.RawMessage
}

func (c *recordingCaller) CallDeviceTool(_ context.Context, name string, args json.RawMessage) (agent.DeviceCallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, recordedCall{name: name, args: append(json.RawMessage(nil), args...)})
	return agent.DeviceCallResult{Content: "synthetic ok"}, nil
}

func (c *recordingCaller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// TestA24ExtraRequiredFieldIsNotExposed reproduces Acceptance report F4: a
// schema with another required parameter cannot be mapped by the volume-only
// executor and must not be bound/exposed.
func TestA24ExtraRequiredFieldIsNotExposed(t *testing.T) {
	loop := agent.NewLoop(2, 1)
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"volume":{"type":"integer","minimum":0,"maximum":30},
			"zone":{"type":"string"}
		},
		"required":["volume","zone"],
		"additionalProperties":false
	}`)
	if _, err := loop.BindVolumeTool("demo.speaker.set_volume", schema); !errors.Is(err, agent.ErrUnsupportedSchema) {
		t.Fatalf("BindVolumeTool error = %v, want ErrUnsupportedSchema", err)
	}
	if _, ok := loop.SyntheticTool(); ok {
		t.Fatal("unsupported extra-required schema was exposed")
	}
}

// TestA24OptionalFieldIsAllowed verifies strictness does not over-reject a
// legal optional field: only the required volume parameter is sent.
func TestA24OptionalFieldIsAllowed(t *testing.T) {
	loop := agent.NewLoop(2, 1)
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"volume":{"type":"integer","minimum":0,"maximum":30},
			"zone":{"type":"string","description":"optional synthetic zone"}
		},
		"required":["volume"],
		"additionalProperties":false
	}`)
	binding, err := loop.BindVolumeTool("demo.speaker.set_volume", schema)
	if err != nil {
		t.Fatalf("BindVolumeTool: %v", err)
	}
	caller := &recordingCaller{}
	ex := agent.NewVolumeExecutor(binding, caller)
	if _, err := ex.Execute(context.Background(), agent.ToolCall{
		Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20}`),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if caller.count() != 1 {
		t.Fatalf("caller calls = %d", caller.count())
	}
	if got := string(caller.calls[0].args); got != `{"volume":20}` {
		t.Fatalf("device args = %s, want only the mapped volume", got)
	}
	if caller.calls[0].name != "demo.speaker.set_volume" {
		t.Fatalf("device tool = %q", caller.calls[0].name)
	}
}

// TestA24RestrictedEnumIsEnforced reproduces the second half of F4: an enum
// must be exposed to the model and enforced locally, not widened to min..max.
func TestA24RestrictedEnumIsEnforced(t *testing.T) {
	loop := agent.NewLoop(2, 1)
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"volume":{"type":"integer","minimum":0,"maximum":100,"enum":[0,50,100]}},
		"required":["volume"],
		"additionalProperties":false
	}`)
	binding, err := loop.BindVolumeTool("demo.speaker.set_volume", schema)
	if err != nil {
		t.Fatalf("BindVolumeTool: %v", err)
	}
	if len(binding.Enum) != 3 || binding.Enum[0] != 0 || binding.Enum[1] != 50 || binding.Enum[2] != 100 {
		t.Fatalf("binding enum = %v", binding.Enum)
	}
	tool, ok := loop.SyntheticTool()
	if !ok {
		t.Fatal("enum schema was not exposed")
	}
	var spec struct {
		InputSchema struct {
			Properties struct {
				Level struct {
					Enum []int `json:"enum"`
				} `json:"level"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(tool, &spec); err != nil {
		t.Fatalf("synthetic tool JSON: %v", err)
	}
	if got := spec.InputSchema.Properties.Level.Enum; len(got) != 3 || got[1] != 50 {
		t.Fatalf("synthetic tool enum = %v", got)
	}

	caller := &recordingCaller{}
	ex := agent.NewVolumeExecutor(binding, caller)
	if _, err := ex.Execute(context.Background(), agent.ToolCall{
		Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":20}`),
	}); !errors.Is(err, agent.ErrInvalidArguments) {
		t.Fatalf("Execute(level=20) error = %v, want ErrInvalidArguments", err)
	}
	if caller.count() != 0 {
		t.Fatalf("invalid enum value reached the device %d time(s)", caller.count())
	}
	if _, err := ex.Execute(context.Background(), agent.ToolCall{
		Name: agent.SyntheticToolName, Arguments: json.RawMessage(`{"level":50}`),
	}); err != nil {
		t.Fatalf("Execute(level=50): %v", err)
	}
	if caller.count() != 1 || string(caller.calls[0].args) != `{"volume":50}` {
		t.Fatalf("device call = %+v", caller.calls)
	}
}

func TestA24MalformedOrOutOfRangeEnumIsRejected(t *testing.T) {
	loop := agent.NewLoop(2, 1)
	cases := []string{
		`{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30,"enum":[]}},"required":["volume"]}`,
		`{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30,"enum":["loud"]}},"required":["volume"]}`,
		`{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30,"enum":[0,99]}},"required":["volume"]}`,
		`{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":30,"enum":{}}},"required":["volume"]}`,
	}
	for i, schema := range cases {
		if _, err := loop.BindVolumeTool("demo.speaker.set_volume", json.RawMessage(schema)); !errors.Is(err, agent.ErrUnsupportedSchema) {
			t.Fatalf("case %d: BindVolumeTool error = %v, want ErrUnsupportedSchema", i, err)
		}
	}
}
