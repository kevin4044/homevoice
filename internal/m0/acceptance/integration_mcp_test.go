package acceptance_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/simulator"
)

// TestIntegratedDeviceMCPAndModelToolLoop exercises the session-bound device
// MCP + model tool loop integration: discovery over MQTT, only the synthetic
// volume tool exposed to the model, execution mapped to the discovered device
// tool, and a confirmed result.
func TestIntegratedDeviceMCPAndModelToolLoop(t *testing.T) {
	h := newHarness(t, nil, nil)
	dev := h.connect("dev-a")
	h.hello("dev-a")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, binding, err := h.probe.DiscoverDeviceTools(ctx, "dev-a")
	if err != nil {
		t.Fatalf("DiscoverDeviceTools: %v", err)
	}
	if len(tools) == 0 || binding.DeviceTool != "demo.speaker.set_volume" {
		t.Fatalf("tools/binding = %+v / %+v", tools, binding)
	}

	model, err := simulator.NewUpstream(simulator.UpstreamOptions{
		UseToolCall: true,
		VolumeLevel: 20,
		ModelText:   "合成音量调用已完成",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.probe.RunAgentTurn(ctx, "dev-a", model)
	if err != nil {
		t.Fatalf("RunAgentTurn: %v", err)
	}
	if !res.Executed {
		t.Fatalf("model turn did not confirm execution: %+v", res)
	}
	if res.Requests > 2 || res.ToolCallsMade > 1 {
		t.Fatalf("model loop exceeded bounds: %+v", res)
	}
	if model.ChatCalls() > 2 {
		t.Fatalf("model calls = %d, want <=2", model.ChatCalls())
	}
	calls := dev.MCP().Calls()
	if len(calls) != 1 {
		t.Fatalf("device MCP calls = %d, want 1", len(calls))
	}
	var args struct {
		Volume *int `json:"volume"`
	}
	if err := json.Unmarshal(calls[0].Args, &args); err != nil || args.Volume == nil || *args.Volume != 20 {
		t.Fatalf("device tool args = %s", calls[0].Args)
	}
	if calls[0].Name != "demo.speaker.set_volume" {
		t.Fatalf("device tool name = %q", calls[0].Name)
	}
}
