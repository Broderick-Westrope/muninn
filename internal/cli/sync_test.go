package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/broderick-westrope/muninn/internal/config"
	"github.com/broderick-westrope/muninn/internal/launchd"
)

// recordingLaunchctl records calls so a repair can be detected.
type recordingLaunchctl struct {
	stubLaunchctl
	calls        []string
	bootstrapErr error
}

func (r *recordingLaunchctl) Bootout(label string) error {
	r.calls = append(r.calls, "bootout")
	return nil
}

func (r *recordingLaunchctl) Bootstrap(path string) error {
	r.calls = append(r.calls, "bootstrap")
	return r.bootstrapErr
}

// repairCmd returns a command whose output is captured, plus the buffer.
func repairCmd() (*cobra.Command, *bytes.Buffer) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	return cmd, &buf
}

func testCfg() *config.Config {
	return &config.Config{Sync: config.SyncConfig{IntervalMinutes: 60, RetryIntervalMinutes: 10}}
}

// TestRepairAgentIgnoresDifferentButPresentBinary is the guard against
// self-inflicting the very bug the repair exists to fix: repointing the
// agent at whatever binary happens to run `sync` would let a `go run` or
// scratch build capture the schedule, and those paths vanish on exit.
func TestRepairAgentIgnoresDifferentButPresentBinary(t *testing.T) {
	other := filepath.Join(t.TempDir(), "muninn")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plistPath := installPlist(t, other)
	before, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}

	lc := &recordingLaunchctl{}
	cmd, buf := repairCmd()
	repairAgentAt(cmd, testCfg(), lc, plistPath)

	if len(lc.calls) != 0 {
		t.Errorf("launchctl calls = %v, want none: an existing binary must not be replaced", lc.calls)
	}
	after, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("plist was rewritten for a binary that still exists")
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want silence", buf.String())
	}
}

// The repair that should happen: the plist names a binary an upgrade
// deleted, so launchd is spawning a path that no longer exists.
func TestRepairAgentFixesMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "Cellar", "muninn", "1.2.3", "bin", "muninn")
	plistPath := installPlist(t, missing)

	lc := &recordingLaunchctl{}
	cmd, buf := repairCmd()
	repairAgentAt(cmd, testCfg(), lc, plistPath)

	if len(lc.calls) == 0 {
		t.Fatal("no launchctl calls; the agent was not repaired")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := launchd.ProgramPath(plist)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(bin) != filepath.Clean(self) {
		t.Errorf("repaired plist points at %q, want this binary %q", bin, self)
	}
	if !strings.Contains(buf.String(), missing) {
		t.Errorf("output = %q, want the missing path explained", buf.String())
	}
}

// A failed bootstrap leaves no agent loaded at all, and the plist on disk
// still looks healthy, so the message has to say so explicitly.
func TestRepairAgentReportsFailedRepair(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "muninn")
	plistPath := installPlist(t, missing)

	lc := &recordingLaunchctl{bootstrapErr: errors.New("bootstrap boom")}
	cmd, buf := repairCmd()
	repairAgentAt(cmd, testCfg(), lc, plistPath)

	out := buf.String()
	if !strings.Contains(out, "no sync is scheduled") {
		t.Errorf("output = %q, want it stated that nothing is scheduled now", out)
	}
	if !strings.Contains(out, "sync --install") {
		t.Errorf("output = %q, want the remedy", out)
	}
}

func TestRepairAgentNoAgentInstalled(t *testing.T) {
	lc := &recordingLaunchctl{}
	cmd, buf := repairCmd()
	repairAgentAt(cmd, testCfg(), lc, filepath.Join(t.TempDir(), "missing.plist"))

	if len(lc.calls) != 0 {
		t.Errorf("launchctl calls = %v, want none when no agent is installed", lc.calls)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want silence", buf.String())
	}
}

// The scheduled job passes --no-repair, so the plist must keep carrying it:
// without it a scheduled run could boot itself out mid-sync.
func TestPlistSuppressesRepair(t *testing.T) {
	plist, err := launchd.Render("/opt/homebrew/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(plist), "--no-repair") {
		t.Errorf("plist does not pass --no-repair:\n%s", plist)
	}
}
