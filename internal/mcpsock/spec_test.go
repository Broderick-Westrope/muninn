package mcpsock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// specFixture writes a fake muninn binary and returns the arguments for
// NewSpec, rooted at a short directory so socket paths fit.
type specFixture struct {
	muninn, index, status, mirrors, state string
}

func newSpecFixture(t *testing.T) specFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "spec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	bin := filepath.Join(root, "muninn")
	if err := os.WriteFile(bin, []byte("binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return specFixture{
		muninn:  bin,
		index:   filepath.Join(root, "data", "index"),
		status:  filepath.Join(root, "state", "status.json"),
		mirrors: filepath.Join(root, "data", "mirrors"),
		state:   filepath.Join(root, "state"),
	}
}

func (f specFixture) spec(t *testing.T) Spec {
	t.Helper()
	s, err := NewSpec(f.muninn, f.index, f.status, f.mirrors, f.state)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	return s
}

func TestSpecIdentityIsStable(t *testing.T) {
	f := newSpecFixture(t)
	if a, b := f.spec(t), f.spec(t); a.Identity != b.Identity {
		t.Errorf("identities differ for identical inputs: %q, %q", a.Identity, b.Identity)
	}
}

func TestSpecIdentityChanges(t *testing.T) {
	tests := map[string]func(t *testing.T, f *specFixture){
		"different index":       func(t *testing.T, f *specFixture) { f.index += "2" },
		"different status file": func(t *testing.T, f *specFixture) { f.status += "2" },
		"different mirrors":     func(t *testing.T, f *specFixture) { f.mirrors += "2" },
		"different state dir":   func(t *testing.T, f *specFixture) { f.state += "2" },
		"rebuilt binary": func(t *testing.T, f *specFixture) {
			if err := os.WriteFile(f.muninn, []byte("binary v2, longer"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"touched binary": func(t *testing.T, f *specFixture) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(f.muninn, later, later); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newSpecFixture(t)
			before := f.spec(t)
			change(t, &f)
			if after := f.spec(t); after.Identity == before.Identity {
				t.Errorf("identity unchanged: %q", after.Identity)
			}
		})
	}
}

func TestSpecResolvesSymlinkedBinary(t *testing.T) {
	f := newSpecFixture(t)
	direct := f.spec(t)

	link := filepath.Join(filepath.Dir(f.muninn), "bin-link")
	if err := os.Symlink(f.muninn, link); err != nil {
		t.Fatal(err)
	}
	f.muninn = link
	viaLink := f.spec(t)

	if viaLink.Identity != direct.Identity {
		t.Errorf("identity through symlink %q, want %q", viaLink.Identity, direct.Identity)
	}
	if viaLink.Muninn != direct.Muninn {
		t.Errorf("Muninn through symlink = %q, want the resolved %q", viaLink.Muninn, direct.Muninn)
	}
}

func TestSpecMakesPathsAbsolute(t *testing.T) {
	f := newSpecFixture(t)
	t.Chdir(filepath.Dir(f.state))
	s, err := NewSpec(f.muninn, "data/index", "state/status.json", "data/mirrors", "state")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	for name, p := range map[string]string{
		"IndexDir": s.IndexDir, "StatusPath": s.StatusPath, "MirrorsDir": s.MirrorsDir,
		"StateDir": s.StateDir, "Socket": s.Socket(), "Lock": s.Lock(), "Log": s.Log(),
	} {
		if !filepath.IsAbs(p) {
			t.Errorf("%s = %q, want an absolute path", name, p)
		}
	}
	if want := f.spec(t).Identity; s.Identity != want {
		t.Errorf("identity from relative paths %q, want %q", s.Identity, want)
	}
}

func TestSpecSocketPathTooLong(t *testing.T) {
	f := newSpecFixture(t)
	f.state = filepath.Join(f.state, strings.Repeat("x", 100))
	if _, err := NewSpec(f.muninn, f.index, f.status, f.mirrors, f.state); !errors.Is(err, ErrSocketPathTooLong) {
		t.Errorf("NewSpec err = %v, want ErrSocketPathTooLong", err)
	}
}

func TestSpecRoundTrip(t *testing.T) {
	s := newSpecFixture(t).spec(t)
	got, err := DecodeSpec(s.Encode())
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if got != s {
		t.Errorf("round trip = %+v, want %+v", got, s)
	}
}
