package launchd

import (
	"errors"
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
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
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
