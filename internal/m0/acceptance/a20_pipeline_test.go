package acceptance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/notify"
	"github.com/homevoice/homevoice/internal/m0/probe"
)

// pipelineGateUpstream lets one pipeline block in ASR and exit later, while a
// second pipeline is already running. It deliberately ignores context for the
// first call to simulate a slow upstream that returns late.
type pipelineGateUpstream struct {
	mu      sync.Mutex
	calls   int
	started chan int
	release [2]chan struct{}
	once    [2]sync.Once
}

func newPipelineGateUpstream() *pipelineGateUpstream {
	u := &pipelineGateUpstream{started: make(chan int, 8)}
	u.release[0] = make(chan struct{})
	u.release[1] = make(chan struct{})
	return u
}

func (u *pipelineGateUpstream) releaseRound(n int) {
	if n < 1 || n > 2 {
		return
	}
	u.once[n-1].Do(func() { close(u.release[n-1]) })
}

func (u *pipelineGateUpstream) Transcribe(_ context.Context, _ []int16) (string, error) {
	u.mu.Lock()
	u.calls++
	n := u.calls
	u.mu.Unlock()
	u.started <- n
	switch n {
	case 1:
		<-u.release[0]
		return "", errors.New("old pipeline ASR exited late")
	case 2:
		<-u.release[1]
		return "new synthetic transcript", nil
	default:
		return "extra synthetic transcript", nil
	}
}

func (u *pipelineGateUpstream) Synthesize(_ context.Context, _ string) ([][]byte, error) {
	return [][]byte{{0x01, 0x02, 0x03}}, nil
}

func recvInt(t *testing.T, ch <-chan int, timeout time.Duration) int {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(timeout):
		t.Fatal("timed out waiting for pipeline start")
		return 0
	}
}

// TestA20OldPipelineMustNotClearNewBusy reproduces F1: an aborted old
// ASR pipeline exiting late must not clear the newer pipeline's busy state, so
// idle notify must still return busy. Both same-session and re-HELLO cases are
// covered.
func TestA20OldPipelineMustNotClearNewBusy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rehello bool
	}{
		{name: "same_session"},
		{name: "rehello", rehello: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newPipelineGateUpstream()
			t.Cleanup(func() {
				gate.releaseRound(1)
				gate.releaseRound(2)
			})
			h := newHarness(t, nil, func(o *probe.Options) {
				o.Upstream = gate
			})
			dev := h.connect("dev-a")
			h.hello("dev-a")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			startRound := func() {
				if err := dev.SendListen("start", "manual"); err != nil {
					t.Fatalf("listen start: %v", err)
				}
				waitFor(t, "listening", 3*time.Second, func() bool {
					s, _ := h.probe.Snapshot("dev-a")
					return s.Listening
				})
				before := h.probe.Stats().UplinkFrames
				pcm := audio.Sine(audio.UplinkParams().SampleRate, 440, 0.2, audio.UplinkParams().FrameSamples())
				if err := dev.SendPCM(pcm); err != nil {
					t.Fatalf("send PCM: %v", err)
				}
				waitFor(t, "uplink audio", 3*time.Second, func() bool {
					return h.probe.Stats().UplinkFrames > before
				})
				if err := dev.SendListen("stop", ""); err != nil {
					t.Fatalf("listen stop: %v", err)
				}
			}

			// Round 1 enters ASR and blocks.
			startRound()
			if n := recvInt(t, gate.started, 5*time.Second); n != 1 {
				t.Fatalf("first pipeline call = %d", n)
			}

			// Cancel round 1 but keep it blocked in the upstream.
			if err := dev.SendAbort("wake_word_detected"); err != nil {
				t.Fatalf("abort: %v", err)
			}
			if tc.rehello {
				if _, err := dev.SendHello(ctx, "udp", 3); err != nil {
					t.Fatalf("re-HELLO: %v", err)
				}
				h.waitSession("dev-a")
			}

			// Round 2 starts and blocks in ASR.
			startRound()
			if n := recvInt(t, gate.started, 5*time.Second); n != 2 {
				t.Fatalf("second pipeline call = %d", n)
			}
			if snap, _ := h.probe.Snapshot("dev-a"); !snap.Busy {
				t.Fatal("new pipeline is not busy before old completion")
			}
			if stats := h.probe.Stats(); stats.PendingTasks == 0 {
				t.Fatal("pending tasks under-report the running new pipeline")
			}
			if _, err := h.probe.Notify("dev-a"); !errors.Is(err, notify.ErrBusy) {
				t.Fatalf("notify while new pipeline busy = %v, want ErrBusy", err)
			}

			// Let the canceled old round exit late. It must not clear round 2.
			gate.releaseRound(1)
			time.Sleep(150 * time.Millisecond)
			if snap, _ := h.probe.Snapshot("dev-a"); !snap.Busy {
				t.Fatal("old pipeline completion cleared the newer round's busy state")
			}
			if stats := h.probe.Stats(); stats.PendingTasks == 0 {
				t.Fatal("old pipeline completion made PendingTasks under-report the new pipeline")
			}
			if _, err := h.probe.Notify("dev-a"); !errors.Is(err, notify.ErrBusy) {
				t.Fatalf("notify after old completion = %v, want ErrBusy", err)
			}

			// Let round 2 finish and return to idle.
			gate.releaseRound(2)
			waitFor(t, "new pipeline idle", 5*time.Second, func() bool {
				s, _ := h.probe.Snapshot("dev-a")
				return !s.Busy
			})
		})
	}
}
