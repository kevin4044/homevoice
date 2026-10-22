package broker

import (
	"errors"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/config"
	"github.com/homevoice/homevoice/internal/m0/limits"
)

// TestBrokerOptionsPolicy verifies the broker's own configuration guard
// rails: bind policy, identity/topic validation, bounded options and
// configurable downlink QoS. A27 covers the process-level entry points; this
// test protects the package contract those entry points rely on.
func TestBrokerOptionsPolicy(t *testing.T) {
	cfg := testConfig(t)

	base := func() Options {
		return Options{
			BindAddr: cfg.Network.MQTTBind,
			Devices:  cfg.Devices,
		}
	}

	rejected := []struct {
		name   string
		mutate func(*Options)
	}{
		{"wildcard bind rejected", func(o *Options) { o.BindAddr = "0.0.0.0:0" }},
		{"empty host bind rejected", func(o *Options) { o.BindAddr = ":1883" }},
		{"public bind rejected", func(o *Options) { o.BindAddr = "8.8.8.8:1883" }},
		{"unlisted private bind rejected", func(o *Options) { o.BindAddr = "192.168.7.7:1883" }},
		{"missing devices rejected", func(o *Options) { o.Devices = nil }},
		{"more than probe limit rejected", func(o *Options) {
			extra := o.Devices[0]
			extra.DeviceID = "dev-c"
			extra.ClientID = "client-c"
			extra.Username = "user-c"
			extra.SubscribeTopic = "devices/p2p/c"
			o.Devices = append(o.Devices, extra)
		}},
		{"duplicate client id rejected", func(o *Options) {
			extra := o.Devices[1]
			extra.DeviceID = "dev-c"
			extra.Username = "user-c"
			extra.SubscribeTopic = "devices/p2p/c"
			o.Devices = append(o.Devices, extra)
		}},
		{"duplicate username rejected", func(o *Options) {
			extra := o.Devices[1]
			extra.DeviceID = "dev-c"
			extra.ClientID = "client-c"
			extra.SubscribeTopic = "devices/p2p/c"
			o.Devices = append(o.Devices, extra)
		}},
		{"wildcard publish topic rejected", func(o *Options) {
			o.Devices = cloneDevices(cfg.Devices)
			o.Devices[0].PublishTopic = "device/#"
		}},
		{"wildcard subscribe topic rejected", func(o *Options) {
			o.Devices = cloneDevices(cfg.Devices)
			o.Devices[0].SubscribeTopic = "devices/p2p/+"
		}},
		{"publish equals own downlink rejected", func(o *Options) {
			o.Devices = cloneDevices(cfg.Devices)
			o.Devices[0].PublishTopic = o.Devices[0].SubscribeTopic
		}},
		{"qos2 downlink rejected", func(o *Options) { o.PublishQoS = 2 }},
		{"qos0/qos1 conflict rejected", func(o *Options) { o.PublishQoS = 1; o.PublishQoSZero = true }},
		{"max packet above probe bound rejected", func(o *Options) {
			o.MaxPacketBytes = limits.MaxMQTTPacketBytes + 1
		}},
		{"connect timeout above probe bound rejected", func(o *Options) {
			o.ConnectTimeout = limits.MQTTConnectTimeout + 1
		}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			b, err := New(opts)
			if err == nil {
				_ = b.Close()
				t.Fatal("New accepted an invalid configuration")
			}
		})
	}

	// Default downlink is QoS1; PublishQoSZero makes it configurable to QoS0.
	t.Run("default downlink qos1", func(t *testing.T) {
		b, _ := startBroker(t, cfg, nil)
		client, incoming := connectDevice(t, b.Addr(), cfg.Devices[0])
		subscribe(t, client, cfg.Devices[0].SubscribeTopic, 1)
		if err := b.Publish(cfg.Devices[0].DeviceID, []byte("qos-default")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := waitPahoMessage(t, incoming, 3*time.Second).Qos(); got != 1 {
			t.Fatalf("default downlink QoS = %d, want 1", got)
		}
	})

	t.Run("configurable downlink qos0", func(t *testing.T) {
		b, _ := startBroker(t, cfg, func(o *Options) { o.PublishQoSZero = true })
		client, incoming := connectDevice(t, b.Addr(), cfg.Devices[0])
		subscribe(t, client, cfg.Devices[0].SubscribeTopic, 1)
		if err := b.Publish(cfg.Devices[0].DeviceID, []byte("qos-zero")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := waitPahoMessage(t, incoming, 3*time.Second).Qos(); got != 0 {
			t.Fatalf("configured downlink QoS = %d, want 0", got)
		}
	})

	t.Run("oversized downlink payload rejected", func(t *testing.T) {
		b, _ := startBroker(t, cfg, nil)
		client, _ := connectDevice(t, b.Addr(), cfg.Devices[0])
		subscribe(t, client, cfg.Devices[0].SubscribeTopic, 1)
		err := b.Publish(cfg.Devices[0].DeviceID, make([]byte, limits.MaxMQTTPacketBytes))
		if !errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("oversized Publish error = %v, want ErrPayloadTooLarge", err)
		}
	})
}

func cloneDevices(devices []config.Device) []config.Device {
	return append([]config.Device(nil), devices...)
}
