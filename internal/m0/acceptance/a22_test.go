package acceptance_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/simulator"
)

// TestA22ProbeSessionRotationIgnoresLateMCPResponse reproduces the probe-level
// A22 correlation defect: after a same-connection HELLO rotation, a delayed
// response for the old MCP request id must not complete a request issued by the
// new session.
func TestA22ProbeSessionRotationIgnoresLateMCPResponse(t *testing.T) {
	firstInit := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondInit := make(chan struct{})
	releaseSecond := make(chan struct{})

	var mu sync.Mutex
	initCalls := 0
	server := simulator.NewMCPServer(simulator.MCPOptions{
		BeforeRespond: func(method string) {
			if method != "initialize" {
				return
			}
			mu.Lock()
			initCalls++
			n := initCalls
			mu.Unlock()
			switch n {
			case 1:
				close(firstInit)
				<-releaseFirst
			case 2:
				close(secondInit)
				<-releaseSecond
			}
		},
	})

	h := newHarnessWithDeviceMutator(t, nil, nil, func(_ string, opts *simulator.DeviceOptions) {
		opts.MCPServer = server
	})
	dev := h.connect("dev-a")
	h.hello("dev-a")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := h.probe.DiscoverDeviceTools(ctx, "dev-a")
		firstDone <- err
	}()
	select {
	case <-firstInit:
	case <-time.After(5 * time.Second):
		t.Fatal("first initialize request did not reach the device")
	}

	// Rotate the audio session on the same MQTT connection while the first
	// initialize is still held by the simulated device.
	oldSession, _ := h.probe.SessionSnapshot("dev-a")
	if _, err := dev.SendHello(ctx, "udp", 3); err != nil {
		t.Fatalf("second HELLO: %v", err)
	}
	h.waitSession("dev-a")
	newSession, _ := h.probe.SessionSnapshot("dev-a")
	if newSession.SessionID == oldSession.SessionID {
		t.Fatal("session was not rotated")
	}
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("first discover succeeded across session rotation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first discover did not cancel after HELLO rotation")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, _, err := h.probe.DiscoverDeviceTools(ctx, "dev-a")
		secondDone <- err
	}()
	select {
	case <-secondInit:
	case <-time.After(5 * time.Second):
		t.Fatal("second initialize request did not reach the device")
	}

	// Release the old initialize response. It carries the id of the rotated
	// session and must not complete the new session's pending initialize.
	close(releaseFirst)
	select {
	case err := <-secondDone:
		t.Fatalf("new-session discover completed from stale old-session response: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	close(releaseSecond)
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("new-session discover failed after its own response: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new-session discover did not complete with its own response")
	}

	mu.Lock()
	calls := initCalls
	mu.Unlock()
	if calls < 2 {
		t.Fatalf("device saw %d initialize requests, want at least 2", calls)
	}
}
