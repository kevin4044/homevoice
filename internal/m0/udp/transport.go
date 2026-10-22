package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/homevoice/homevoice/internal/m0/limits"
)

// ErrClosed reports use of a closed transport.
var ErrClosed = errors.New("udp transport closed")

// Transport is one UDP socket used for both receiving and sending. Replies
// MUST use this same socket so the device sees a stable source port/address.
type Transport struct {
	conn   *net.UDPConn
	closed chan struct{}
}

// Listen opens an isolated UDP socket. Bind policy (loopback/authorized
// private only) is enforced by the caller via config.ValidateBindAddress.
func Listen(bind string) (*Transport, error) {
	addr, err := net.ResolveUDPAddr("udp", bind)
	if err != nil {
		return nil, fmt.Errorf("resolve udp bind %q: %w", bind, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %q: %w", bind, err)
	}
	return &Transport{conn: conn, closed: make(chan struct{})}, nil
}

// LocalAddr returns the bound UDP address.
func (t *Transport) LocalAddr() *net.UDPAddr {
	if t == nil || t.conn == nil {
		return nil
	}
	return t.conn.LocalAddr().(*net.UDPAddr)
}

// SendTo writes one bounded datagram on the receive socket.
func (t *Transport) SendTo(addr *net.UDPAddr, b []byte) error {
	if t == nil || t.conn == nil {
		return ErrClosed
	}
	if addr == nil {
		return errors.New("udp send: nil peer")
	}
	if len(b) > limits.MaxUDPDatagramBytes {
		return fmt.Errorf("%w: send %d > %d", ErrPacketTooLarge, len(b), limits.MaxUDPDatagramBytes)
	}
	select {
	case <-t.closed:
		return ErrClosed
	default:
	}
	_, err := t.conn.WriteToUDP(b, addr)
	return err
}

// ReadFrom reads one datagram directly (used by tests and simple callers).
func (t *Transport) ReadFrom(b []byte) (int, *net.UDPAddr, error) {
	if t == nil || t.conn == nil {
		return 0, nil, ErrClosed
	}
	return t.conn.ReadFromUDP(b)
}

// SetReadDeadline sets the read deadline on the socket.
func (t *Transport) SetReadDeadline(tm time.Time) error {
	if t == nil || t.conn == nil {
		return ErrClosed
	}
	return t.conn.SetReadDeadline(tm)
}

// Close closes the socket. It is idempotent.
func (t *Transport) Close() error {
	if t == nil {
		return nil
	}
	select {
	case <-t.closed:
		return nil
	default:
		close(t.closed)
	}
	return t.conn.Close()
}

// RunReadLoop reads bounded datagrams until ctx is cancelled or the socket is
// closed. Datagrams larger than the probe limit are truncated to
// MaxUDPDatagramBytes+1 bytes and handed to the handler so it can reject them;
// the loop never allocates per-datagram beyond the fixed receive buffer.
func (t *Transport) RunReadLoop(ctx context.Context, handler func(from *net.UDPAddr, datagram []byte)) error {
	if t == nil || t.conn == nil {
		return ErrClosed
	}
	buf := make([]byte, limits.MaxUDPDatagramBytes+1)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := t.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			return err
		}
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-t.closed:
				return nil
			default:
			}
			return err
		}
		datagram := make([]byte, n)
		copy(datagram, buf[:n])
		handler(from, datagram)
	}
}
