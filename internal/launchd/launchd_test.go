package launchd

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type stubLaunchctl struct {
	calls        []string
	bootoutErr   error
	bootstrapErr error
}

func (s *stubLaunchctl) Bootout(label string) error {
	s.calls = append(s.calls, "bootout "+label)
	return s.bootoutErr
}

func (s *stubLaunchctl) Bootstrap(plistPath string) error {
	s.calls = append(s.calls, "bootstrap "+plistPath)
	return s.bootstrapErr
}

func TestRenderGolden(t *testing.T) {
	got, err := Render(
		"/usr/local/bin/muninn",
		"/Users/test/.config/muninn/config.json",
		"/Users/test/.local/state/muninn/sync.log",
		60,
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "plist.golden"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("rendered plist does not match golden file\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderDefaultsInterval(t *testing.T) {
	got, err := Render("/bin/muninn", "/cfg.json", "/log", 0)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := Render("/bin/muninn", "/cfg.json", "/log", 60)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != string(want) {
		t.Error("interval 0 did not default to 60 minutes")
	}
}

func TestInstall(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), "agents", Label+".plist")
	// Bootout fails as it does when the job is not loaded; install must
	// ignore it and proceed to bootstrap.
	lc := &stubLaunchctl{bootoutErr: errors.New("not loaded")}

	if err := Install(lc, plistPath, []byte("plist-content")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading installed plist: %v", err)
	}
	if string(data) != "plist-content" {
		t.Errorf("plist content = %q, want %q", data, "plist-content")
	}
	want := []string{"bootout " + Label, "bootstrap " + plistPath}
	if !reflect.DeepEqual(lc.calls, want) {
		t.Errorf("launchctl calls = %v, want %v", lc.calls, want)
	}

	// Idempotent: a second install rewrites and reloads without error.
	if err := Install(lc, plistPath, []byte("plist-content")); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !reflect.DeepEqual(lc.calls, append(want, want...)) {
		t.Errorf("launchctl calls after reinstall = %v", lc.calls)
	}
}

func TestInstallBootstrapError(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), Label+".plist")
	lc := &stubLaunchctl{bootstrapErr: errors.New("bootstrap boom")}

	if err := Install(lc, plistPath, []byte("x")); err == nil {
		t.Fatal("Install with failing bootstrap: want error, got nil")
	}
}

func TestUninstall(t *testing.T) {
	plistPath := filepath.Join(t.TempDir(), Label+".plist")
	if err := os.WriteFile(plistPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing plist: %v", err)
	}
	lc := &stubLaunchctl{}

	if err := Uninstall(lc, plistPath); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plist still exists after uninstall (err = %v)", err)
	}
	want := []string{"bootout " + Label}
	if !reflect.DeepEqual(lc.calls, want) {
		t.Errorf("launchctl calls = %v, want %v", lc.calls, want)
	}

	// Idempotent: uninstalling again (no plist, job not loaded) succeeds.
	lc.bootoutErr = errors.New("not loaded")
	if err := Uninstall(lc, plistPath); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
}

func TestInstalledNotInstalled(t *testing.T) {
	_, err := Installed(filepath.Join(t.TempDir(), "missing.plist"))
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want to wrap fs.ErrNotExist", err)
	}
}

func TestInstalledRoundTrip(t *testing.T) {
	// Render is deterministic, so byte equality against the installed
	// plist is what lets callers detect an agent that has drifted from
	// the current binary and config.
	plistPath := filepath.Join(t.TempDir(), "agents", Label+".plist")
	want, err := Render("/usr/local/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := Install(&stubLaunchctl{}, plistPath, want); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := Installed(plistPath)
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("installed plist does not match the rendered bytes")
	}

	stale, err := Render("/opt/homebrew/Cellar/muninn/1.2.3/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Equal(got, stale) {
		t.Error("a plist rendered from a different binary path compared equal")
	}
}

func TestProgramPath(t *testing.T) {
	plist, err := Render("/opt/homebrew/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got, err := ProgramPath(plist)
	if err != nil {
		t.Fatalf("ProgramPath: %v", err)
	}
	if got != "/opt/homebrew/bin/muninn" {
		t.Errorf("ProgramPath = %q, want the binary path, not a later argument", got)
	}
}

func TestProgramPathRejectsMalformed(t *testing.T) {
	tests := map[string]string{
		"no key":       `<plist><dict></dict></plist>`,
		"no entries":   `<key>ProgramArguments</key><array></array>`,
		"unterminated": `<key>ProgramArguments</key><array><string>/bin/muninn`,
	}
	for name, plist := range tests {
		if _, err := ProgramPath([]byte(plist)); err == nil {
			t.Errorf("%s: err = nil, want an error", name)
		}
	}
}

func TestNeedsRepair(t *testing.T) {
	want, err := Render("/opt/homebrew/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	plistPath := filepath.Join(t.TempDir(), "agent.plist")

	if _, err := NeedsRepair(plistPath, want); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled when no agent exists", err)
	}

	if err := os.WriteFile(plistPath, want, 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := NeedsRepair(plistPath, want)
	if err != nil {
		t.Fatalf("NeedsRepair: %v", err)
	}
	if stale {
		t.Error("stale = true for an agent matching the rendered plist")
	}

	// The upgrade case: the installed plist still names the old versioned
	// path while the running binary renders the stable symlink.
	old, err := Render("/opt/homebrew/Cellar/muninn/1.2.3/bin/muninn", "/cfg.json", "/log", 10)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := os.WriteFile(plistPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err = NeedsRepair(plistPath, want)
	if err != nil {
		t.Fatalf("NeedsRepair: %v", err)
	}
	if !stale {
		t.Error("stale = false for an agent pointing at a moved binary")
	}

	// A schedule change must also be detected, so editing the config and
	// re-running sync repairs the agent without an explicit reinstall.
	if err := os.WriteFile(plistPath, want, 0o644); err != nil {
		t.Fatal(err)
	}
	retimed, err := Render("/opt/homebrew/bin/muninn", "/cfg.json", "/log", 30)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	stale, err = NeedsRepair(plistPath, retimed)
	if err != nil {
		t.Fatalf("NeedsRepair: %v", err)
	}
	if !stale {
		t.Error("stale = false after the tick interval changed")
	}
}
