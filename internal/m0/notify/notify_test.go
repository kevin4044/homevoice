package notify

import (
	"errors"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

type fakePublisher struct {
	online    map[string]uint64
	published []struct {
		device  string
		payload []byte
	}
	publishErr error
}

func (f *fakePublisher) Online(deviceID string) (uint64, bool) {
	gen, ok := f.online[deviceID]
	return gen, ok
}

func (f *fakePublisher) Publish(deviceID string, payload []byte) error {
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, struct {
		device  string
		payload []byte
	}{deviceID, append([]byte(nil), payload...)})
	return nil
}

// TestA20IdleNotifyDoesNotRequireUDP covers the idle-notify contract: MQTT
// online + no UDP session can publish, busy/offline fail explicitly, and
// published is never reported as played.
func TestA20IdleNotifyDoesNotRequireUDP(t *testing.T) {
	pub := &fakePublisher{online: map[string]uint64{"dev-a": 3}}
	clk := clock.NewFake(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	svc, err := New(pub, clk, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Notify("dev-a", "Homevoice 测试通知", "https://test-audio.invalid/notice.wav")
	if err != nil {
		t.Fatalf("Notify = %v", err)
	}
	if res.Status != StatusPublished || res.Generation != 3 || res.Played {
		t.Fatalf("result = %+v", res)
	}
	if len(pub.published) != 1 || pub.published[0].device != "dev-a" {
		t.Fatalf("published = %+v", pub.published)
	}
	n, err := wire.ParseNotify(pub.published[0].payload)
	if err != nil {
		t.Fatalf("published payload = %v", err)
	}
	if n.Text != "Homevoice 测试通知" || n.AudioURL != "https://test-audio.invalid/notice.wav" {
		t.Fatalf("notify payload = %+v", n)
	}
}

func TestA20BusyAndOfflineFailExplicitly(t *testing.T) {
	pub := &fakePublisher{online: map[string]uint64{"dev-a": 1, "dev-b": 1}}
	busy := func(deviceID string) bool { return deviceID == "dev-a" }
	svc, err := New(pub, clock.NewFake(time.Now()), busy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Notify("dev-a", "hello", "https://test-audio.invalid/x.wav"); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy notify error = %v, want ErrBusy", err)
	}
	if len(pub.published) != 0 {
		t.Fatal("busy device received a forced notification")
	}
	delete(pub.online, "dev-b")
	res, err := svc.Notify("dev-b", "hello", "https://test-audio.invalid/x.wav")
	if !errors.Is(err, ErrOffline) || res.Status != StatusOffline {
		t.Fatalf("offline notify = %+v, %v", res, err)
	}
	if len(pub.published) != 0 {
		t.Fatal("offline device publish was attempted")
	}
}

func TestNotifyPublishFailureAndInvalidURL(t *testing.T) {
	pub := &fakePublisher{online: map[string]uint64{"dev-a": 1}, publishErr: errors.New("synthetic broker failure")}
	svc, err := New(pub, clock.NewFake(time.Now()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Notify("dev-a", "hello", "https://test-audio.invalid/x.wav")
	if err == nil || res.Status != StatusFailed || res.Played {
		t.Fatalf("publish failure = %+v, %v", res, err)
	}
	pub.publishErr = nil
	if _, err := svc.Notify("dev-a", "hello", "http://test-audio.invalid/x.wav"); err == nil {
		t.Fatal("plaintext audio URL accepted")
	}
	if len(pub.published) != 0 {
		t.Fatal("invalid notify was published")
	}
}

func TestNotifyRequiresPublisher(t *testing.T) {
	if _, err := New(nil, nil, nil, nil); err == nil {
		t.Fatal("New accepted a nil publisher")
	}
}
