package simulator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/homevoice/homevoice/internal/m0/audio"
)

// AudioServer is a synthetic HTTPS audio endpoint for isolated notify/media
// tests. It serves a generated WAV tone at /notice.wav with a runtime
// self-signed certificate; clients must trust the returned certificate pool.
// It is test infrastructure, not a production media service.
type AudioServer struct {
	ln      net.Listener
	srv     *http.Server
	url     string
	certPEM []byte
}

// SyntheticWAV returns a small mono 16-bit PCM WAV containing a sine tone.
// It is generated locally and contains no recorded/private audio.
func SyntheticWAV(sampleRate, durationMS int) []byte {
	if sampleRate <= 0 || sampleRate > 48000 {
		sampleRate = 16000
	}
	if durationMS <= 0 || durationMS > 5000 {
		durationMS = 300
	}
	samples := sampleRate * durationMS / 1000
	pcm := audio.Sine(sampleRate, 440, 0.2, samples)
	data := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	const headerLen = 44
	out := make([]byte, headerLen+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:34], 2)                    // block align
	binary.LittleEndian.PutUint16(out[34:36], 16)                   // bits/sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(data)))
	copy(out[44:], data)
	return out
}

// NewAudioServer starts an isolated loopback HTTPS server. It never binds a
// public interface and never contacts another service.
func NewAudioServer() (*AudioServer, error) {
	certPEM, keyPEM, cert, leaf, err := selfSignedCertificate()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	tlsLn := tls.NewListener(ln, tlsCfg)
	wav := SyntheticWAV(16000, 300)
	mux := http.NewServeMux()
	mux.HandleFunc("/notice.wav", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(wav)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() { _ = srv.Serve(tlsLn) }()
	_ = leaf
	_ = keyPEM
	return &AudioServer{
		ln:      tlsLn,
		srv:     srv,
		url:     "https://" + ln.Addr().String() + "/notice.wav",
		certPEM: certPEM,
	}, nil
}

// URL returns the HTTPS media URL.
func (s *AudioServer) URL() string { return s.url }

// CertPEM returns the self-signed certificate PEM (test trust anchor).
func (s *AudioServer) CertPEM() []byte { return append([]byte(nil), s.certPEM...) }

// Client returns an HTTP client that verifies the server certificate against
// this server's CA. It never disables verification.
func (s *AudioServer) Client() (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.certPEM) {
		return nil, fmt.Errorf("simulator: invalid test certificate pool")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
		},
	}, nil
}

// Close stops the server.
func (s *AudioServer) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	_ = s.ln.Close()
	return s.srv.Close()
}

func selfSignedCertificate() (certPEM, keyPEM []byte, cert tls.Certificate, leaf *x509.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "homevoice-m0-test-audio"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, tls.Certificate{}, nil, err
	}
	return certPEM, keyPEM, cert, leaf, nil
}
