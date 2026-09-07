package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/broderick-westrope/muninn/internal/launchd"
	"github.com/broderick-westrope/muninn/internal/status"
)

// stubLaunchctl answers the loaded probe without a real launchd.
type stubLaunchctl struct {
	loaded    bool
	loadedErr error
}

func (stubLaunchctl) Bootout(string) error   { return nil }
func (stubLaunchctl) Bootstrap(string) error { return nil }

func (s stubLaunchctl) Loaded(string) (bool, error) { return s.loaded, s.loadedErr }

// loadedCtl is a launchctl reporting a healthy, registered job.
func loadedCtl() stubLaunchctl { return stubLaunchctl{loaded: true} }

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
	got := describeAgent(loadedCtl(), filepath.Join(t.TempDir(), "missing.plist"))
	if !strings.HasPrefix(got, "not installed") {
		t.Errorf("state = %q, want the not-installed message", got)
	}
}

func TestDescribeAgentHealthy(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got := describeAgent(loadedCtl(), installPlist(t, self))
	if !strings.HasPrefix(got, "installed (") || strings.Contains(got, "—") {
		t.Errorf("state = %q, want a plain installed message", got)
	}
}

// TestDescribeAgentNotLoaded covers the state Install can leave behind: a
// bootstrap that fails after the plist is written leaves a file identical
// to a healthy install, so only launchd can reveal that nothing is
// scheduled.
func TestDescribeAgentNotLoaded(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got := describeAgent(stubLaunchctl{loaded: false}, installPlist(t, self))
	if !strings.Contains(got, "BROKEN") {
		t.Errorf("state = %q, want it flagged BROKEN", got)
	}
	if !strings.Contains(got, "not loaded") {
		t.Errorf("state = %q, want the unloaded job named as the cause", got)
	}
}

func TestDescribeAgentLoadedProbeFails(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got := describeAgent(stubLaunchctl{loadedErr: errors.New("launchctl missing")}, installPlist(t, self))
	if !strings.Contains(got, "could not confirm") {
		t.Errorf("state = %q, want the unknown-loaded-state message", got)
	}
}

// The failure this reports is the one a Homebrew upgrade causes: the plist
// still names a versioned Cellar path that no longer exists, so launchd
// spawns nothing, forever, without complaint.
func TestDescribeAgentBrokenBinaryPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Cellar", "muninn", "1.2.3", "bin", "muninn")
	got := describeAgent(loadedCtl(), installPlist(t, missing))
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
	// An existing but different binary: worth reporting, not broken, and
	// deliberately not auto-repaired.
	other := filepath.Join(t.TempDir(), "muninn")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := describeAgent(loadedCtl(), installPlist(t, other))
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
	got := describeAgent(loadedCtl(), plistPath)
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
	for _, want := range []string{"last run:", "last clean sweep:", "bad credentials", "consecutive failures: 71"} {
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
	if !strings.Contains(buf.String(), "last clean sweep: never") {
		t.Errorf("output should say a success has never happened:\n%s", buf.String())
	}
}

func TestPrintStatusFutureClock(t *testing.T) {
	var buf bytes.Buffer
	err := printStatus(&buf, &status.SyncStatus{
		StartedAt:     time.Now(),
		FinishedAt:    time.Now().Add(48 * time.Hour),
		LastSuccessAt: time.Now().Add(48 * time.Hour),
		Success:       true,
		Repos:         map[string]status.RepoStatus{},
	})
	if err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "dated in the future") {
		t.Errorf("output should flag a backwards clock rather than print a negative age:\n%s", buf.String())
	}
}

// A gc failure leaves the repo fully indexed, so it is reported apart from
// real failures and must not appear as one.
func TestPrintStatusSeparatesGCFailures(t *testing.T) {
	var buf bytes.Buffer
	err := printStatus(&buf, &status.SyncStatus{
		StartedAt:     time.Now().Add(-time.Minute),
		FinishedAt:    time.Now(),
		LastSuccessAt: time.Now(),
		Success:       true,
		Repos: map[string]status.RepoStatus{
			"acme/a": {Fetched: true, Indexed: true, GCError: "gc boom"},
		},
	})
	if err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "result: success") {
		t.Errorf("a gc-only failure must not make the run a failure:\n%s", out)
	}
	if !strings.Contains(out, "GC WARNING") || !strings.Contains(out, "gc boom") {
		t.Errorf("output missing the gc section:\n%s", out)
	}
}
