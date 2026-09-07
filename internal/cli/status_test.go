package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/launchd"
	"github.com/broderick-westrope/muninn/internal/status"
)

// installPlist writes a plist for binPath at the returned path.
func installPlist(t *testing.T, binPath string) string {
	t.Helper()
	plistPath := filepath.Join(t.TempDir(), launchd.Label+".plist")
	plist, err := launchd.Render(binPath, "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	return plistPath
}

func TestDescribeAgentNotInstalled(t *testing.T) {
	got := describeAgent(filepath.Join(t.TempDir(), "missing.plist"))
	if !strings.HasPrefix(got, "not installed") {
		t.Errorf("state = %q, want the not-installed message", got)
	}
}

func TestDescribeAgentHealthy(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got := describeAgent(installPlist(t, self))
	if !strings.HasPrefix(got, "installed (") || strings.Contains(got, "—") {
		t.Errorf("state = %q, want a plain installed message", got)
	}
}

// The failure this reports is the one a Homebrew upgrade causes: the plist
// still names a versioned Cellar path that no longer exists, so launchd
// spawns nothing, forever, without complaint.
func TestDescribeAgentBrokenBinaryPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Cellar", "muninn", "1.2.3", "bin", "muninn")
	got := describeAgent(installPlist(t, missing))
	if !strings.Contains(got, "BROKEN") {
		t.Errorf("state = %q, want it flagged BROKEN", got)
	}
	if !strings.Contains(got, missing) {
		t.Errorf("state = %q, want the missing path named", got)
	}
	if !strings.Contains(got, "sync --install") {
		t.Errorf("state = %q, want the remedy", got)
	}
}

func TestDescribeAgentDifferentBinary(t *testing.T) {
	// An existing but different binary: worth reporting, not broken.
	other := filepath.Join(t.TempDir(), "muninn")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := describeAgent(installPlist(t, other))
	if strings.Contains(got, "BROKEN") {
		t.Errorf("state = %q, should not be BROKEN when the binary exists", got)
	}
	if !strings.Contains(got, "not this binary") {
		t.Errorf("state = %q, want the mismatch reported", got)
	}
}

func TestDescribeAgentUnrecognizedPlist(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(plistPath, []byte("<plist></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := describeAgent(plistPath)
	if !strings.Contains(got, "unrecognized contents") {
		t.Errorf("state = %q, want the parse failure reported", got)
	}
}

// printStatus must show the attempt clock and the success clock
// separately: a run that fails hourly keeps "last run" recent, and only
// the success line reveals that the index has stopped updating.
func TestPrintStatusSeparatesClocks(t *testing.T) {
	var buf bytes.Buffer
	err := printStatus(&buf, &status.SyncStatus{
		StartedAt:           time.Now().Add(-2 * time.Minute),
		FinishedAt:          time.Now().Add(-time.Minute),
		LastSuccessAt:       time.Now().Add(-72 * time.Hour),
		Success:             false,
		Error:               "discovering repos: bad credentials",
		ConsecutiveFailures: 71,
		Repos:               map[string]status.RepoStatus{"acme/a": {Fetched: true, Indexed: true}},
	})
	if err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"last run:", "last success:", "bad credentials", "consecutive failures: 71"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintStatusNeverSucceeded(t *testing.T) {
	var buf bytes.Buffer
	err := printStatus(&buf, &status.SyncStatus{
		StartedAt:  time.Now().Add(-time.Minute),
		FinishedAt: time.Now(),
		Repos:      map[string]status.RepoStatus{},
	})
	if err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "last success: never") {
		t.Errorf("output should say a success has never happened:\n%s", buf.String())
	}
}
