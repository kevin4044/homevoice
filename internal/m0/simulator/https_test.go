package simulator

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPSAudioServerServesWAVOverVerifiedTLS(t *testing.T) {
	srv, err := NewAudioServer()
	if err != nil {
		t.Fatalf("NewAudioServer: %v", err)
	}
	defer srv.Close()
	client, err := srv.Client()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(srv.URL())
	if err != nil {
		t.Fatalf("verified GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/wav") {
		t.Fatalf("content type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("not a WAV body (%d bytes)", len(body))
	}
	if !bytes.Equal(body[:4], []byte("RIFF")) {
		t.Fatal("missing RIFF header")
	}

	// A normal trust store must reject the self-signed test certificate; this
	// proves the server is not advertising an insecure client path.
	plain := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	if _, err := plain.Get(srv.URL()); err == nil {
		t.Fatal("default-trust client accepted the self-signed test certificate")
	}

	// Unknown media paths are not served.
	badURL := strings.TrimSuffix(srv.URL(), "/notice.wav") + "/other.wav"
	resp2, err := client.Get(badURL)
	if err != nil {
		t.Fatalf("404 GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", resp2.StatusCode)
	}
}

func TestSyntheticWAVIsBoundedAndSynthetic(t *testing.T) {
	wav := SyntheticWAV(16000, 100)
	const want = 44 + 1600*2 // 100 ms at 16 kHz mono PCM16
	if len(wav) != want {
		t.Fatalf("WAV length = %d, want %d", len(wav), want)
	}
	if len(SyntheticWAV(0, 0)) == 0 {
		t.Fatal("default WAV is empty")
	}
}
