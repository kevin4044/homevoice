// Package notify implements the M0-v0.1 idle MQTT notification path.
//
// Requirements: notification is delivered over the authenticated MQTT downlink
// and MUST NOT require an existing UDP audio session. A busy or offline device
// fails explicitly; the probe does not force an interrupt, queue unbounded
// notifications or auto-replay. "published" never means "played".
package notify

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/homevoice/homevoice/internal/m0/clock"
	"github.com/homevoice/homevoice/internal/m0/logging"
	"github.com/homevoice/homevoice/internal/m0/wire"
)

// Status is the explicit outcome of one notification attempt.
type Status string

const (
	// StatusPublished means the broker accepted the MQTT publish. It does not
	// prove device download or playback.
	StatusPublished Status = "published"
	// StatusBusy means the device is in an active audio round; notify is
	// rejected rather than forcing an interrupt.
	StatusBusy Status = "busy"
	// StatusOffline means no authenticated MQTT connection exists.
	StatusOffline Status = "offline"
	// StatusFailed means the publish path failed after the device was online.
	StatusFailed Status = "failed"
)

// Errors for explicit busy/offline outcomes.
var (
	ErrOffline = errors.New("device offline: notify not sent")
	ErrBusy    = errors.New("device busy: notify rejected without interruption")
)

// Result documents one attempt. Played is always false: the probe cannot prove
// playback from an MQTT publish.
type Result struct {
	DeviceID       string
	Status         Status
	Generation     uint64
	SentAt         time.Time
	Played         bool
	PublishedBytes int
	Reason         string
}

// Publisher is the authenticated MQTT online/publish surface the notifier
// needs. It deliberately has no UDP session concept.
type Publisher interface {
	Online(deviceID string) (generation uint64, ok bool)
	Publish(deviceID string, payload []byte) error
}

// BusyFunc reports whether a device is currently in an audio round.
type BusyFunc func(deviceID string) bool

// Service sends idle notifications.
type Service struct {
	pub  Publisher
	clk  clock.Clock
	busy BusyFunc
	log  *slog.Logger
}

// New creates a notifier. pub and clk are required. busy may be nil (idle is
// assumed); it must be a cheap non-blocking check.
func New(pub Publisher, clk clock.Clock, busy BusyFunc, logger *slog.Logger) (*Service, error) {
	if pub == nil {
		return nil, errors.New("notify: publisher is required")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if logger == nil {
		logger = logging.Discard()
	}
	return &Service{pub: pub, clk: clk, busy: busy, log: logger}, nil
}

// Notify sends one synthetic notification to a device that is online and idle.
// No UDP session is needed or consulted.
func (s *Service) Notify(deviceID, text, audioURL string) (Result, error) {
	gen, online := s.pub.Online(deviceID)
	if !online {
		return Result{DeviceID: deviceID, Status: StatusOffline, Reason: "no authenticated MQTT connection"}, ErrOffline
	}
	if s.busy != nil && s.busy(deviceID) {
		return Result{DeviceID: deviceID, Status: StatusBusy, Generation: gen, Reason: "device is in an active audio round"}, ErrBusy
	}
	payload, err := wire.EncodeNotify(text, audioURL)
	if err != nil {
		return Result{DeviceID: deviceID, Status: StatusFailed, Generation: gen, Reason: err.Error()}, fmt.Errorf("notify payload: %w", err)
	}
	if err := s.pub.Publish(deviceID, payload); err != nil {
		return Result{DeviceID: deviceID, Status: StatusFailed, Generation: gen, Reason: "mqtt publish failed"}, fmt.Errorf("notify publish: %w", err)
	}
	res := Result{
		DeviceID:       deviceID,
		Status:         StatusPublished,
		Generation:     gen,
		SentAt:         s.clk.Now(),
		Played:         false,
		PublishedBytes: len(payload),
	}
	s.log.Info("notify published (playback not confirmed)", "device", deviceID, "bytes", len(payload))
	return res, nil
}
