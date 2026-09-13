//go:build integration

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/platformfake"
)

// freePort reserves a port and releases it for the child to bind. Racy in
// principle, acceptable for a single-job CI.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// withoutEnv drops the named variables from env — used so this test proves
// the binary derives its identity from instance metadata regardless of
// what the parent process happens to have set, not merely because nothing
// filled MWS_PROJECT/MWS_ZONE this time.
func withoutEnv(env []string, names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	filtered := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !drop[key] {
			filtered = append(filtered, kv)
		}
	}
	return filtered
}

func TestSmoke_BinaryStartsServesHealthAndShutsDownCleanly(t *testing.T) {
	f := platformfake.New(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "mws-iops-autoscaler")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	addr := freePort(t)
	cmd := exec.Command(bin,
		"-config", "testdata/smoke-config.yaml",
		"-state-dir", filepath.Join(dir, "state"),
		"-journal", filepath.Join(dir, "journal.jsonl"),
		"-listen", addr,
	)
	// MWS_PROJECT and MWS_ZONE are deliberately excluded, even if the
	// parent process happens to have them set: local mode must derive both
	// from the fake VM's own instance metadata.
	cmd.Env = append(withoutEnv(os.Environ(), "MWS_PROJECT", "MWS_ZONE"),
		"MWS_BASE_ENDPOINT="+f.BaseURL(),
		"MWS_COMPUTE_METADATA_HOST="+f.MetadataURL(),
		"MWS_TOKEN=smoke-token",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the listener rather than sleeping a fixed interval.
	deadline := time.Now().Add(30 * time.Second)
	var healthz *http.Response
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			healthz = resp
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if healthz == nil {
		t.Fatal("binary never served /healthz")
	}
	defer healthz.Body.Close()
	if healthz.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", healthz.StatusCode)
	}

	// runAgent starts the HTTP listener in a goroutine before calling
	// agent.Run, which performs initial discovery, so a 200 from /healthz
	// does not mean discovery has run yet. Poll /readyz until
	// discovery_succeeded flips rather than reading it once and racing the
	// agent.
	type readyBody struct {
		Ready              bool `json:"ready"`
		ConfigValid        bool `json:"config_valid"`
		DiscoverySucceeded bool `json:"discovery_succeeded"`
		SourceReachable    bool `json:"source_reachable"`
	}
	var body readyBody
	var status int
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var b readyBody
		decodeErr := json.NewDecoder(resp.Body).Decode(&b)
		status = resp.StatusCode
		resp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode /readyz: %v", decodeErr)
		}
		body = b
		if b.DiscoverySucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !body.DiscoverySucceeded {
		t.Fatal("discovery_succeeded never became true — the binary never reached the fake Compute API")
	}
	if !body.ConfigValid {
		t.Error("config_valid = false, want true")
	}
	// No /dev/disk/by-id/mws-* exists on a runner, so every disk fails guest-path
	// verification, the managed set is empty and readiness must say so.
	if body.SourceReachable {
		t.Error("source_reachable = true, but no /dev/disk/by-id/mws-* exists on a runner")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 while source_reachable is false", status)
	}

	metrics, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", metrics.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("binary exited with %v, want a clean exit on SIGTERM", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("binary did not exit within 15s of SIGTERM")
	}
}
