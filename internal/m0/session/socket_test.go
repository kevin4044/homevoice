package session

import (
	"bytes"
	"context"
	"math/rand"
	"net"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/udp"
)

// TestA12OutboundUsesSameSocket verifies that Manager replies with the same
// UDP socket that received the device frame (stable source port), using a real
// loopback transport.
func TestA12OutboundUsesSameSocket(t *testing.T) {
	server, err := udp.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	remote, err := udp.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	inbound := make(chan struct{}, 1)
	m, err := NewManager(Options{
		Clock:          clock.Real{},
		Rand:           rand.New(rand.NewSource(13)),
		Send:           server.SendTo,
		OnFrame:        func(InboundFrame) {},
		OnSessionEnded: func(SessionEnd) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.RunReadLoop(ctx, func(from *net.UDPAddr, datagram []byte) {
			if _, err := m.HandleDatagram(from, datagram); err == nil {
				select {
				case inbound <- struct{}{}:
				default:
				}
			}
		})
	}()

	info, err := m.Hello("dev-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.SendTo(server.LocalAddr(), buildFrame(t, info, 1, 60, []byte("uplink"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inbound:
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not accept inbound frame")
	}
	if err := m.SendAudio("dev-a", []byte("downlink")); err != nil {
		t.Fatalf("SendAudio = %v", err)
	}

	buf := make([]byte, 1600)
	_ = remote.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := remote.ReadFrom(buf)
	if err != nil {
		t.Fatalf("remote read: %v", err)
	}
	if from.String() != server.LocalAddr().String() {
		t.Fatalf("downlink source = %v, want receive socket %v", from, server.LocalAddr())
	}
	h, plain, err := udp.Decrypt(keyBytes(t, info), buf[:n])
	if err != nil {
		t.Fatalf("decrypt downlink: %v", err)
	}
	if h.Sequence != 1 || !bytes.Equal(plain, []byte("downlink")) {
		t.Fatalf("downlink packet seq=%d payload=%q", h.Sequence, plain)
	}
}
