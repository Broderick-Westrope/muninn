package web

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// editorBinaries maps a configured editor scheme to its CLI executable. The
// scheme doubles as a URL scheme for the client-side fallback, but it is not
// an executable name: VS Code's CLI is "code". Without this map every
// vscode user would silently take the fallback path forever.
var editorBinaries = map[string]string{
	"cursor": "cursor",
	"vscode": "code",
}

// ErrEditorCLINotFound reports that the editor CLI is absent from PATH.
// Routine rather than exceptional: it is a separately-installed shell
// command that many users never set up, so the client falls back to the URL
// scheme instead of surfacing an error.
var ErrEditorCLINotFound = errors.New("editor CLI not found on PATH; install the editor's shell command to open files with the repo loaded")

// launchEditor opens file at line in an editor window with dir loaded as the
// workspace folder — the whole point of shelling out, since the cursor:// and
// vscode:// URL schemes cannot express a workspace folder and drop the file
// into whichever window was last focused.
//
// The argv is fixed and every element is built by the caller from
// server-side state; nothing is interpolated into a shell. No --new-window:
// given a bare folder argument the CLI focuses an existing window already
// holding that folder and spawns one only if none exists, which is the
// intent without accumulating a window per click.
func launchEditor(scheme, dir, file string, line int) error {
	bin, ok := editorBinaries[scheme]
	if !ok {
		return fmt.Errorf("unknown editor scheme %q", scheme)
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s: %w", bin, ErrEditorCLINotFound)
	}
	cmd := exec.Command(path, dir, "--goto", fmt.Sprintf("%s:%d", file, line))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching %s: %w", bin, err)
	}
	// Reap the child so it never lingers as a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// resolveInCheckout resolves a repo-relative path inside a checkout and
// verifies it stays within it, returning the resolved checkout root and the
// resolved target.
//
// Both sides go through EvalSymlinks so a symlink inside the checkout cannot
// redirect the launch outside it, and the returned paths are the resolved
// ones: what gets validated is what gets opened. Containment is compared on
// a path-separator boundary, so a checkout at /dev/repo does not admit
// /dev/repo-other.
//
// A missing file yields fs.ErrNotExist, which is routine rather than
// exceptional: the local checkout is frequently on a different commit than
// the index.
func resolveInCheckout(checkoutDir, relPath string) (root, target string, err error) {
	root, err = filepath.EvalSymlinks(checkoutDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving checkout %s: %w", checkoutDir, err)
	}
	target, err = filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relPath)))
	if err != nil {
		return "", "", err
	}
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes checkout %s", relPath, root)
	}
	return root, target, nil
}
