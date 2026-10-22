package udp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

func TestTransportLoopbackReadAndSendSameSocket(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(server) = %v", err)
	}
	defer server.Close()
	remote, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(remote) = %v", err)
	}
	defer remote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan struct {
		from net.Addr
		data []byte
	}, 4)
	go func() {
		_ = server.RunReadLoop(ctx, func(from *net.UDPAddr, datagram []byte) {
			received <- struct {
				from net.Addr
				data []byte
			}{from, append([]byte(nil), datagram...)}
		})
	}()

	if err := remote.SendTo(server.LocalAddr(), []byte("ping")); err != nil {
		t.Fatalf("remote.SendTo = %v", err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got.data, []byte("ping")) {
			t.Fatalf("server received %q", got.data)
		}
		if got.from.String() != remote.LocalAddr().String() {
			t.Fatalf("server saw source %v, want %v", got.from, remote.LocalAddr())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive datagram")
	}

	// Reply on the same socket; the device must see the server's bound port.
	if err := server.SendTo(remote.LocalAddr(), []byte("pong")); err != nil {
		t.Fatalf("server.SendTo = %v", err)
	}
	reply := make(chan struct {
		from net.Addr
		data []byte
	}, 1)
	replyCtx, replyCancel := context.WithCancel(context.Background())
	defer replyCancel()
	go func() {
		_ = remote.RunReadLoop(replyCtx, func(from *net.UDPAddr, datagram []byte) {
			select {
			case reply <- struct {
				from net.Addr
				data []byte
			}{from, append([]byte(nil), datagram...)}:
			default:
			}
		})
	}()
	select {
	case got := <-reply:
		if string(got.data) != "pong" {
			t.Fatalf("remote received %q", got.data)
		}
		if got.from.String() != server.LocalAddr().String() {
			t.Fatalf("reply source %v, want same-socket %v", got.from, server.LocalAddr())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote did not receive reply")
	}
}

func TestTransportRejectsOversizeAndClosedSend(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	if err := server.SendTo(remote.LocalAddr(), make([]byte, limits.MaxUDPDatagramBytes+1)); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("oversize send error = %v, want ErrPacketTooLarge", err)
	}
	if err := server.SendTo(remote.LocalAddr(), make([]byte, limits.MaxUDPDatagramBytes)); err != nil {
		t.Fatalf("max-size send = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := server.SendTo(remote.LocalAddr(), []byte("x")); err != ErrClosed {
		t.Fatalf("send after close = %v, want ErrClosed", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestTransportReadLoopStopsOnContextCancel(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.RunReadLoop(ctx, func(*net.UDPAddr, []byte) {})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunReadLoop = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not stop after context cancel")
	}
}
