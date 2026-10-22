package acceptance_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/homevoice/homevoice/internal/m0/audio"
	"github.com/homevoice/homevoice/internal/m0/fixtures"
)

// TestA01CleanRepositoryToolchain pins the offline evidence baseline: a fresh
// new repository with no retired-project dependency/path, locked public
// dependencies, readable fixtures, a usable Go toolchain and native libopus.
func TestA01CleanRepositoryToolchain(t *testing.T) {
	root, err := fixtures.RepoRoot()
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	mod := string(goMod)
	if !strings.Contains(mod, "module github.com/homevoice/homevoice") {
		t.Fatalf("go.mod module line missing:\n%s", mod)
	}
	if strings.Contains(mod, "replace") {
		t.Fatal("go.mod has a replace directive; probe must not depend on external/local copies")
	}
	allowed := []string{
		"github.com/mochi-mqtt/server/v2",
		"github.com/eclipse/paho.mqtt.golang",
		"github.com/gorilla/websocket",
		"golang.org/x/net",
		"golang.org/x/sync",
		"github.com/rs/xid",
	}
	requireBlock := mod
	if i := strings.Index(mod, "require"); i >= 0 {
		requireBlock = mod[i:]
	}
	for _, dep := range allowed {
		if !strings.Contains(requireBlock, dep) {
			t.Fatalf("expected locked dependency %q in go.mod:\n%s", dep, mod)
		}
	}
	// No dependency outside the audited public set (guards accidental copies).
	allowedSet := ""
	for _, dep := range allowed {
		allowedSet += dep + "\n"
	}
	for _, line := range strings.Split(requireBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "require") || line == ")" {
			continue
		}
		modPath := strings.Fields(line)[0]
		if modPath == "github.com/homevoice/homevoice" {
			continue
		}
		ok := false
		for _, dep := range allowed {
			if modPath == dep {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("unaudited dependency %q in go.mod (allowed:\n%s)", modPath, allowedSet)
		}
	}

	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("Makefile: %v", err)
	}
	for _, target := range []string{"verify-m0:", "verify-m0-live:", "verify-m0-hardware:", "fmt-check:", "vet:", "test:"} {
		if !strings.Contains(string(makefile), target) {
			t.Fatalf("Makefile missing required target %q", target)
		}
	}
	if strings.Contains(string(makefile), "curl ") || strings.Contains(string(makefile), "wget ") {
		t.Fatal("Makefile must not fetch external content as part of verification")
	}

	// Source tree must not encode retired-project paths or local absolute
	// paths. Build the needles by concatenation so this scanner does not
	// match its own source.
	forbidden := []string{
		"/Us" + "ers/",
		"/ho" + "me/",
		"hai" + "men",
		"xiao" + "zhi-esp32-server",
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".cache" || name == "local" || name == "evidence" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, needle := range forbidden {
			if strings.Contains(text, needle) {
				rel, _ := filepath.Rel(root, path)
				t.Fatalf("forbidden retired/local path %q found in %s", needle, rel)
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk source: %v", err)
	}
	if checked < 20 {
		t.Fatalf("only %d Go files scanned; unexpected layout", checked)
	}

	suite, err := fixtures.LoadControlMessages()
	if err != nil {
		t.Fatalf("control fixtures: %v", err)
	}
	if suite.FixtureVersion != "M0-v0.1" {
		t.Fatalf("control fixture version = %q", suite.FixtureVersion)
	}
	vectors, err := fixtures.LoadUDPVectors()
	if err != nil {
		t.Fatalf("udp fixtures: %v", err)
	}
	if vectors.FixtureVersion != "M0-v0.1" || len(vectors.Vectors) == 0 {
		t.Fatalf("udp fixture version/vectors = %q/%d", vectors.FixtureVersion, len(vectors.Vectors))
	}
	if runtime.Version() == "" {
		t.Fatal("empty Go runtime version")
	}
	opus := audio.OpusVersion()
	if opus == "" || strings.Contains(opus, "unavailable") {
		t.Fatalf("libopus unavailable: %q", opus)
	}
	t.Logf("baseline: go=%s os=%s/%s libopus=%s fixtures=%s gofiles=%d",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, opus, suite.FixtureVersion, checked)
}
