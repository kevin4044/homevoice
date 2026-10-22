package broker

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/mochi-mqtt/server/v2/listeners"

	"log/slog"
)

// deadlineListener is a TCP/TLS listener for mochi-mqtt that bounds the MQTT
// CONNECT handshake with an initial read deadline.
//
// mochi reads the first packet before any keepalive deadline is known, so a
// peer that connects and stays silent would otherwise occupy a connection
// forever. The listener sets ReadDeadline before handing the conn to
// Server.EstablishConnection; mochi replaces it with the keepalive deadline
// once the session is established.
//
// It also tracks accepted connections so Broker.Close can close half-open
// handshakes instead of waiting for the connect timeout.
type deadlineListener struct {
	sync.Mutex
	id             string
	address        string
	protocol       string
	tlsConfig      *tls.Config
	connectTimeout time.Duration
	log            *slog.Logger

	ln      net.Listener
	conns   map[net.Conn]struct{}
	ended   bool
	serving bool
	wg      sync.WaitGroup

	closeOnce sync.Once
	initOnce  sync.Once
	initErr   error
}

func newDeadlineListener(id, address string, tlsConfig *tls.Config, connectTimeout time.Duration) *deadlineListener {
	protocol := "tcp"
	if tlsConfig != nil {
		protocol = "tcp+tls"
	}
	return &deadlineListener{
		id:             id,
		address:        address,
		protocol:       protocol,
		tlsConfig:      tlsConfig,
		connectTimeout: connectTimeout,
		log:            slog.New(slog.DiscardHandler),
		conns:          map[net.Conn]struct{}{},
	}
}

// Init binds the network listener. It is idempotent so Broker.New may create
// the socket eagerly and mochi AddListener may call Init again.
func (l *deadlineListener) Init(log *slog.Logger) error {
	l.initOnce.Do(func() {
		if log != nil {
			l.log = log
		}
		if l.tlsConfig != nil {
			l.ln, l.initErr = tls.Listen("tcp", l.address, l.tlsConfig)
		} else {
			l.ln, l.initErr = net.Listen("tcp", l.address)
		}
	})
	return l.initErr
}

// Serve accepts connections until the listener is closed. Each connection gets
// a CONNECT handshake read deadline before the establish callback runs.
func (l *deadlineListener) Serve(establish listeners.EstablishFn) {
	l.Lock()
	if l.serving || l.ended || l.ln == nil {
		l.Unlock()
		return
	}
	l.serving = true
	l.Unlock()

	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}

		l.Lock()
		if l.ended {
			l.Unlock()
			_ = conn.Close()
			return
		}
		l.conns[conn] = struct{}{}
		l.wg.Add(1)
		l.Unlock()

		go func(c net.Conn) {
			defer l.wg.Done()
			defer func() {
				l.Lock()
				delete(l.conns, c)
				l.Unlock()
			}()

			// Bound the CONNECT handshake (and a TLS handshake) before
			// mochi starts reading. Established sessions replace this with
			// their keepalive deadline.
			if l.connectTimeout > 0 {
				_ = c.SetReadDeadline(time.Now().Add(l.connectTimeout))
			}

			if err := establish(l.id, c); err != nil && !errors.Is(err, net.ErrClosed) {
				l.log.Debug("mqtt connection ended", "listener", l.id, "error", err)
			}
		}(conn)
	}
}

// ID returns the listener id used by mochi.
func (l *deadlineListener) ID() string { return l.id }

// Address returns the bound address, including the actual ephemeral port.
func (l *deadlineListener) Address() string {
	l.Lock()
	defer l.Unlock()
	if l.ln != nil {
		return l.ln.Addr().String()
	}
	return l.address
}

// Protocol returns the listener transport for logging.
func (l *deadlineListener) Protocol() string { return l.protocol }

// Close stops accepting, closes every accepted connection (including
// half-open CONNECT handshakes), waits for every connection goroutine to
// finish its broker cleanup, and only then invokes the library's
// close-clients callback.
//
// The ordering matters: mochi's closeListenerClients (the callback) calls
// Clients.GetByListener, which re-locks the clients RWMutex while iterating.
// Doing that concurrently with a connection goroutine deleting its client id
// can wedge the RWMutex (reader-preference inversion). Closing connections
// first and waiting for their attachClient goroutines to return removes that
// concurrent writer before the callback runs.
func (l *deadlineListener) Close(closeClients listeners.CloseFn) {
	l.closeOnce.Do(func() {
		l.Lock()
		l.ended = true
		ln := l.ln
		l.Unlock()

		if ln != nil {
			_ = ln.Close()
		}

		l.Lock()
		pending := make([]net.Conn, 0, len(l.conns))
		for c := range l.conns {
			pending = append(pending, c)
		}
		l.Unlock()
		for _, c := range pending {
			_ = c.Close()
		}

		// attachClient's deferred cleanup (including Clients.Delete) runs
		// before establish returns, so this wait drains those writers.
		l.wg.Wait()

		if closeClients != nil {
			closeClients(l.id)
		}
	})
	l.wg.Wait()
}
