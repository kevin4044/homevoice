package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// DeviceCallResult is the mapped outcome of one device tool call.
type DeviceCallResult struct {
	Content string
	IsError bool
}

// DeviceCaller calls the discovered device tool. Implementations are bound to
// the authenticated device session (device, generation, protocol session) by
// the probe; the model never sees or supplies that target.
type DeviceCaller interface {
	CallDeviceTool(ctx context.Context, name string, args json.RawMessage) (DeviceCallResult, error)
}

// VolumeExecutor is the only bridge from the model-facing synthetic tool to
// the discovered session-bound device tool. It re-validates the call name and
// argument range so a caller bypassing Loop still cannot target another device
// or call another tool.
type VolumeExecutor struct {
	binding ToolBinding
	caller  DeviceCaller
}

// NewVolumeExecutor wraps caller with the discovered binding returned by
// Loop.BindVolumeTool.
func NewVolumeExecutor(binding ToolBinding, caller DeviceCaller) *VolumeExecutor {
	return &VolumeExecutor{binding: binding, caller: caller}
}

// Binding returns the session-bound tool mapping.
func (e *VolumeExecutor) Binding() ToolBinding { return e.binding }

// Execute maps set_current_device_volume(level) onto the discovered device
// tool with its own parameter name.
func (e *VolumeExecutor) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	failed := func(content string) ToolResult {
		return ToolResult{CallID: call.ID, Content: content, IsError: true}
	}
	if e == nil || e.caller == nil {
		return failed("no device caller bound"), fmt.Errorf("%w: nil device caller", ErrInvalidConfig)
	}
	if call.Name != SyntheticToolName {
		return failed("tool not exposed"), fmt.Errorf("%w: %q", ErrInvalidToolCall, call.Name)
	}
	level, err := parseLevel(e.binding, call.Arguments)
	if err != nil {
		return failed(err.Error()), err
	}
	deviceArgs, err := json.Marshal(map[string]int{e.binding.Parameter: level})
	if err != nil {
		return failed("argument mapping failed"), fmt.Errorf("%w: map arguments: %v", ErrInvalidArguments, err)
	}
	res, err := e.caller.CallDeviceTool(ctx, e.binding.DeviceTool, deviceArgs)
	if err != nil {
		return failed(err.Error()), fmt.Errorf("%w: %v", ErrToolFailed, err)
	}
	if res.IsError {
		return ToolResult{CallID: call.ID, Content: res.Content, IsError: true}, nil
	}
	return ToolResult{CallID: call.ID, Content: res.Content}, nil
}

var _ ToolExecutor = (*VolumeExecutor)(nil)
