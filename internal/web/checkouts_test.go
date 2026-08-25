package web

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCheckout creates dir with a fake .git/config declaring originURL
// as the origin remote (no real git needed: ScanCheckouts parses the
// config file directly).
func writeCheckout(t *testing.T, dir, originURL string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", gitDir, err)
	}
	config := `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = ` + originURL + `
	fetch = +refs/heads/*:refs/remotes/origin/*
`
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
		t.Fatalf("writing git config: %v", err)
	}
}

func TestScanCheckoutsURLForms(t *testing.T) {
	root := t.TempDir()
	repos := map[string]string{
		"ssh-scp":    "git@github.com:Acme/Widget.git",
		"ssh-alias":  "git@github.com-work:acme/gadget.git",
		"https":      "https://github.com/acme/thing",
		"https-git":  "https://github.com/acme/other.git",
		"ssh-scheme": "ssh://git@github.com/acme/proto.git",
	}
	for dir, url := range repos {
		writeCheckout(t, filepath.Join(root, dir), url)
	}

	got := ScanCheckouts([]string{root})
	want := map[string]string{
		"acme/widget": filepath.Join(root, "ssh-scp"),
		"acme/gadget": filepath.Join(root, "ssh-alias"),
		"acme/thing":  filepath.Join(root, "https"),
		"acme/other":  filepath.Join(root, "https-git"),
		"acme/proto":  filepath.Join(root, "ssh-scheme"),
	}
	if len(got) != len(want) {
		t.Fatalf("ScanCheckouts = %v, want %v", got, want)
	}
	for key, path := range want {
		if got[key] != path {
			t.Errorf("checkouts[%q] = %q, want %q", key, got[key], path)
		}
	}
}

func TestScanCheckoutsDepthLimit(t *testing.T) {
	root := t.TempDir()
	// Level 1 and level 2 are found; level 3 is beyond the scan depth.
	writeCheckout(t, filepath.Join(root, "one"), "git@github.com:acme/one.git")
	writeCheckout(t, filepath.Join(root, "group", "two"), "git@github.com:acme/two.git")
	writeCheckout(t, filepath.Join(root, "a", "b", "three"), "git@github.com:acme/three.git")

	got := ScanCheckouts([]string{root})
	if got["acme/one"] != filepath.Join(root, "one") {
		t.Errorf("acme/one = %q, want level-1 checkout", got["acme/one"])
	}
	if got["acme/two"] != filepath.Join(root, "group", "two") {
		t.Errorf("acme/two = %q, want level-2 checkout", got["acme/two"])
	}
	if _, ok := got["acme/three"]; ok {
		t.Error("acme/three found, want it skipped beyond depth 2")
	}
}

func TestScanCheckoutsNonGitHubIgnored(t *testing.T) {
	root := t.TempDir()
	writeCheckout(t, filepath.Join(root, "gl"), "git@gitlab.com:acme/widget.git")
	writeCheckout(t, filepath.Join(root, "corp"), "https://git.example.com/acme/widget.git")

	if got := ScanCheckouts([]string{root}); len(got) != 0 {
		t.Errorf("ScanCheckouts = %v, want empty for non-github remotes", got)
	}
}

func TestScanCheckoutsSkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	writeCheckout(t, filepath.Join(root, ".hidden", "repo"), "git@github.com:acme/hidden.git")

	if got := ScanCheckouts([]string{root}); len(got) != 0 {
		t.Errorf("ScanCheckouts = %v, want empty when the checkout is under a hidden dir", got)
	}
}

func TestScanCheckoutsFirstMatchWins(t *testing.T) {
	root := t.TempDir()
	// ReadDir returns entries sorted by name, so "aaa" is visited first.
	writeCheckout(t, filepath.Join(root, "aaa"), "git@github.com:acme/widget.git")
	writeCheckout(t, filepath.Join(root, "bbb"), "git@github.com:acme/widget.git")

	got := ScanCheckouts([]string{root})
	if got["acme/widget"] != filepath.Join(root, "aaa") {
		t.Errorf("acme/widget = %q, want the first match %q", got["acme/widget"], filepath.Join(root, "aaa"))
	}
}

func TestScanCheckoutsNoDescendIntoRepo(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "outer")
	writeCheckout(t, outer, "git@github.com:acme/outer.git")
	writeCheckout(t, filepath.Join(outer, "vendored"), "git@github.com:acme/inner.git")

	got := ScanCheckouts([]string{root})
	if got["acme/outer"] != outer {
		t.Errorf("acme/outer = %q, want %q", got["acme/outer"], outer)
	}
	if _, ok := got["acme/inner"]; ok {
		t.Error("acme/inner found, want no descent into a matched repo")
	}
}

func TestScanCheckoutsGitFile(t *testing.T) {
	root := t.TempDir()
	// A worktree-style checkout: .git is a file pointing at the real
	// gitdir elsewhere.
	worktree := filepath.Join(root, "wt")
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[remote \"origin\"]\n\turl = git@github.com:acme/worktree.git\n"
	if err := os.WriteFile(filepath.Join(gitdir, "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ScanCheckouts([]string{root})
	if got["acme/worktree"] != worktree {
		t.Errorf("acme/worktree = %q, want %q", got["acme/worktree"], worktree)
	}
}

func TestScanCheckoutsMissingRoot(t *testing.T) {
	if got := ScanCheckouts([]string{filepath.Join(t.TempDir(), "does-not-exist")}); len(got) != 0 {
		t.Errorf("ScanCheckouts = %v, want empty for a missing root", got)
	}
}

func TestParseGitHubRepo(t *testing.T) {
	tests := []struct {
		url  string
		want string
		ok   bool
	}{
		{"git@github.com:Acme/Widget.git", "acme/widget", true},
		{"git@github.com-personal:acme/widget.git", "acme/widget", true},
		{"https://github.com/acme/widget", "acme/widget", true},
		{"https://github.com/acme/widget.git", "acme/widget", true},
		{"ssh://git@github.com/acme/widget.git", "acme/widget", true},
		{"git@gitlab.com:acme/widget.git", "", false},
		{"https://git.example.com/acme/widget.git", "", false},
		{"https://github.company.com/acme/widget.git", "", false},
		{"file:///tmp/repo", "", false},
		{"", "", false},
		{"nonsense", "", false},
	}
	for _, tt := range tests {
		got, ok := parseGitHubRepo(tt.url)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseGitHubRepo(%q) = %q, %v; want %q, %v", tt.url, got, ok, tt.want, tt.ok)
		}
	}
}
