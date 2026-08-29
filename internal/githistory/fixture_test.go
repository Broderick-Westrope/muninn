package githistory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fixture file contents referenced by tests across the package.
const (
	fooV1 = "package fixture\n\nfunc Foo() string {\n\treturn \"one\"\n}\n"
	fooV2 = "package fixture\n\n// Foo returns a number word.\nfunc Foo() string {\n\treturn \"one\"\n}\n"
	fooV3 = "package fixture\n\n// Foo returns a number word.\nfunc Foo() string {\n\treturn \"two\"\n}\n"
	docV1 = "docs\n"
	docV2 = "docs\nmore\n"
)

// tabSubject is a commit subject containing tabs, which the log parser
// must preserve.
const tabSubject = "subject\twith\ttab"

// bigLines is sized so a whole-file addition alone exceeds the 64 KiB
// diff budget.
const bigLines = 5000

// bigContent renders the big fixture file; tag distinguishes versions so
// a modification rewrites every line.
func bigContent(tag string) string {
	var b strings.Builder
	for i := range bigLines {
		fmt.Fprintf(&b, "big line %04d %s\n", i, tag)
	}
	return b.String()
}

// fixture describes the shared history fixture: a bare mirror plus the
// SHA of every scripted commit. All commits stay reachable from branches
// (main, feature, divergent, orphan) so clones of the mirror see them.
type fixture struct {
	Mirror string
	Src    string

	Root        string // day 01: foo.go, bar.txt, doc.txt
	TabSubject  string // day 02: doc.txt v2, tab-containing subject
	MagicAdd    string // day 03: introduces the literal MAGIC
	SideChange  string // day 04: C — documents Foo on branch feature
	BarV2       string // day 05: bar.txt v2 on main, authored by Alice
	Merge       string // day 06: M — merges feature with --no-ff
	Rename      string // day 07: git mv bar.txt baz.txt
	PostRename  string // day 08: extends baz.txt
	MagicRemove string // day 09: removes the literal MAGIC
	Lockfile    string // day 10: foo.go v3 + package-lock.json
	Binary      string // day 11: adds bin.dat (NUL bytes)
	Big         string // day 12: adds big.txt (> budget on its own)
	Mixed       string // day 13: rewrites big.txt, adds small.txt
	NonASCII    string // day 14: adds café.txt (non-ASCII path)
	Divergent   string // day 15: branch divergent forked from Root
	Orphan      string // day 16: orphan branch root (disjoint history)
	Head        string // main tip (== NonASCII)
}

// fixGit runs a git command for fixture setup, failing the test on error.
// Extra env entries let commits carry deterministic dates. Global and
// system config are disabled so host configuration (commit templates,
// hooks paths, ...) cannot leak into the fixture history.
func fixGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=test", "-c", "user.email=test@example.com"}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// writeFile writes a fixture file, creating parent directories.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// newFixtureRepo builds the shared history fixture in a scratch repo and
// mirrors it with a bare clone. Every commit gets a distinct, increasing
// author and committer date (2024-01-<day> 12:00 UTC) so date filters are
// deterministic.
func newFixtureRepo(t *testing.T) fixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	fixGit(t, "", nil, "init", "-b", "main", src)

	day := 0
	commit := func(msg string, extra ...string) string {
		day++
		date := fmt.Sprintf("2024-01-%02d 12:00:00 +0000", day)
		env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		args := append([]string{"commit", "-m", msg}, extra...)
		fixGit(t, src, env, args...)
		return fixGit(t, src, nil, "rev-parse", "HEAD")
	}

	f := fixture{Src: src}

	writeFile(t, src, "foo.go", fooV1)
	writeFile(t, src, "bar.txt", "bar v1\n")
	writeFile(t, src, "doc.txt", docV1)
	fixGit(t, src, nil, "add", ".")
	f.Root = commit("root: add foo and bar")

	writeFile(t, src, "doc.txt", docV2)
	fixGit(t, src, nil, "add", ".")
	f.TabSubject = commit(tabSubject)

	writeFile(t, src, "magic.txt", "MAGIC\n")
	fixGit(t, src, nil, "add", ".")
	f.MagicAdd = commit("introduce MAGIC")

	fixGit(t, src, nil, "checkout", "-b", "feature")
	writeFile(t, src, "foo.go", fooV2)
	fixGit(t, src, nil, "add", ".")
	f.SideChange = commit("document Foo")

	fixGit(t, src, nil, "checkout", "main")
	writeFile(t, src, "bar.txt", "bar v2\n")
	fixGit(t, src, nil, "add", ".")
	f.BarV2 = commit("update bar", "--author=Alice <alice@example.com>")

	day++
	mergeDate := fmt.Sprintf("2024-01-%02d 12:00:00 +0000", day)
	fixGit(t, src, []string{"GIT_AUTHOR_DATE=" + mergeDate, "GIT_COMMITTER_DATE=" + mergeDate},
		"merge", "--no-ff", "feature", "-m", "merge feature into main")
	f.Merge = fixGit(t, src, nil, "rev-parse", "HEAD")

	fixGit(t, src, nil, "mv", "bar.txt", "baz.txt")
	f.Rename = commit("rename bar to baz")

	writeFile(t, src, "baz.txt", "bar v2\nbaz addition\n")
	fixGit(t, src, nil, "add", ".")
	f.PostRename = commit("extend baz")

	writeFile(t, src, "magic.txt", "nothing\n")
	fixGit(t, src, nil, "add", ".")
	f.MagicRemove = commit("remove MAGIC")

	writeFile(t, src, "foo.go", fooV3)
	writeFile(t, src, "package-lock.json", "{\n  \"lockfileVersion\": 3\n}\n")
	fixGit(t, src, nil, "add", ".")
	f.Lockfile = commit("change Foo and lockfile")

	writeFile(t, src, "bin.dat", "\x00\x01\x02\x03binary\x00data\n")
	fixGit(t, src, nil, "add", ".")
	f.Binary = commit("add binary")

	writeFile(t, src, "big.txt", bigContent("old"))
	fixGit(t, src, nil, "add", ".")
	f.Big = commit("add big file")

	writeFile(t, src, "big.txt", bigContent("new"))
	writeFile(t, src, "small.txt", "small\n")
	fixGit(t, src, nil, "add", ".")
	f.Mixed = commit("small and big")

	writeFile(t, src, "café.txt", "café v1\n")
	fixGit(t, src, nil, "add", ".")
	f.NonASCII = commit("add café")
	f.Head = f.NonASCII

	fixGit(t, src, nil, "checkout", "-b", "divergent", f.Root)
	writeFile(t, src, "div.txt", "divergent\n")
	fixGit(t, src, nil, "add", ".")
	f.Divergent = commit("divergent work")

	fixGit(t, src, nil, "checkout", "--orphan", "orphan")
	fixGit(t, src, nil, "rm", "-rf", ".")
	writeFile(t, src, "orphan.txt", "orphan\n")
	fixGit(t, src, nil, "add", ".")
	f.Orphan = commit("orphan root")

	fixGit(t, src, nil, "checkout", "main")

	f.Mirror = filepath.Join(t.TempDir(), "repo.git")
	fixGit(t, "", nil, "clone", "--bare", src, f.Mirror)
	return f
}

// shas extracts the SHA column for order-insensitive assertions.
func shas(commits []Commit) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.SHA
	}
	return out
}
