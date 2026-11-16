//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRegression_Build_PrebuiltBinaryIsUsedAndKept(t *testing.T) {
	prebuilt := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(prebuilt, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentBinaryEnv, prebuilt)

	got, cleanup, err := BuildAgentBinary(context.Background(), "..")
	if err != nil {
		t.Fatalf("BuildAgentBinary: %v", err)
	}
	if got != prebuilt {
		t.Fatalf("path = %q, want the prebuilt %q", got, prebuilt)
	}
	cleanup()
	if _, err := os.Stat(prebuilt); err != nil {
		t.Fatalf("cleanup removed the shared prebuilt binary: %v", err)
	}
}

func TestRegression_Build_MissingPrebuiltBinaryFails(t *testing.T) {
	t.Setenv(agentBinaryEnv, filepath.Join(t.TempDir(), "absent"))
	if _, _, err := BuildAgentBinary(context.Background(), ".."); err == nil {
		t.Fatal("BuildAgentBinary succeeded with a missing prebuilt binary")
	}
}

// Scenario jobs run concurrently on one runner machine, so each build needs its own path.
func TestRegression_Build_ConcurrentBuildsDoNotShareAPath(t *testing.T) {
	t.Setenv(agentBinaryEnv, "")
	first, cleanupFirst, err := BuildAgentBinary(context.Background(), "..")
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, cleanupSecond, err := BuildAgentBinary(context.Background(), "..")
	if err != nil {
		cleanupFirst()
		t.Fatalf("second build: %v", err)
	}
	defer cleanupSecond()
	if first == second {
		cleanupFirst()
		t.Fatalf("both builds wrote %q", first)
	}
	cleanupFirst()
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("removing the first build removed the second: %v", err)
	}
}
