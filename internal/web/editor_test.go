package web

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestEditorBinaries(t *testing.T) {
	// There is no executable named "vscode": VS Code's CLI is "code".
	if got := editorBinaries["vscode"]; got != "code" {
		t.Errorf("vscode maps to %q, want code", got)
	}
	if got := editorBinaries["cursor"]; got != "cursor" {
		t.Errorf("cursor maps to %q, want cursor", got)
	}
}

// checkoutFixture builds a checkout containing one file and returns the
// checkout dir alongside its symlink-resolved form (on macOS a t.TempDir
// under /var resolves to /private/var).
func checkoutFixture(t *testing.T) (dir, resolved string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir, resolved
}

func TestResolveInCheckoutOK(t *testing.T) {
	dir, resolved := checkoutFixture(t)

	root, target, err := resolveInCheckout(dir, "main.go")
	if err != nil {
		t.Fatalf("resolveInCheckout: %v", err)
	}
	if root != resolved {
		t.Errorf("root = %q, want the resolved checkout %q", root, resolved)
	}
	if want := filepath.Join(resolved, "main.go"); target != want {
		t.Errorf("target = %q, want %q", target, want)
	}
}

func TestResolveInCheckoutEscape(t *testing.T) {
	dir, _ := checkoutFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	rel, err := filepath.Rel(dir, outside)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if _, _, err := resolveInCheckout(dir, rel); err == nil {
		t.Errorf("resolveInCheckout(%q) succeeded, want an escape error", rel)
	}
}

// TestResolveInCheckoutSymlinkEscape covers the case a plain path-cleaning
// check would miss: the path stays inside the checkout textually, but a
// symlink points out of it.
func TestResolveInCheckoutSymlinkEscape(t *testing.T) {
	dir, _ := checkoutFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}
	link := filepath.Join(dir, "link.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, _, err := resolveInCheckout(dir, "link.go"); err == nil {
		t.Error("resolveInCheckout followed a symlink out of the checkout, want an escape error")
	}
}

// TestResolveInCheckoutSiblingPrefix is the separator-boundary case: a naive
// strings.HasPrefix would let a sibling directory sharing a name prefix pass.
func TestResolveInCheckoutSiblingPrefix(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "repo")
	sibling := filepath.Join(base, "repo-other")
	for _, d := range []string{checkout, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "f.go"), []byte("package f\n"), 0o644); err != nil {
		t.Fatalf("writing sibling file: %v", err)
	}

	if _, _, err := resolveInCheckout(checkout, "../repo-other/f.go"); err == nil {
		t.Error("resolveInCheckout admitted a sibling sharing the name prefix, want an escape error")
	}
}

func TestResolveInCheckoutMissing(t *testing.T) {
	dir, _ := checkoutFixture(t)

	// Routine: the checkout is often on a different commit than the index.
	_, _, err := resolveInCheckout(dir, "absent.go")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestLaunchEditorUnknownScheme(t *testing.T) {
	if err := launchEditor("emacs", t.TempDir(), "f.go", 1); err == nil {
		t.Error("launchEditor with an unknown scheme succeeded, want an error")
	}
}

func TestLaunchEditorCLIMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no editor binary in an empty PATH

	err := launchEditor("cursor", t.TempDir(), "f.go", 1)
	if !errors.Is(err, ErrEditorCLINotFound) {
		t.Errorf("err = %v, want ErrEditorCLINotFound", err)
	}
}
