package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// e2e is a built muninn binary plus an isolated XDG environment, rooted
// under /tmp so socket paths fit macOS's sun_path limit.
type e2e struct {
	bin   string
	root  string
	env   []string
	state string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the muninn binary")
	}
	root, err := os.MkdirTemp("/tmp", "mcpe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	bin := filepath.Join(root, "muninn")
	build := exec.Command("go", "build", "-o", bin, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	e := &e2e{bin: bin, root: root, state: filepath.Join(root, "state", "muninn")}
	e.env = append(os.Environ(),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"MUNINN_MCP_IDLE=500ms",
	)
	cfg := filepath.Join(root, "config", "muninn", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"connections":{"gh":{"type":"github","repos":["acme/widget"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data", "muninn", "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	return e
}

// session starts `muninn mcp` with args the way an MCP client would.
func (e *e2e) session(t *testing.T, args ...string) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command(e.bin, append([]string{"mcp"}, args...)...)
	cmd.Env = e.env
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e"}, nil)
	cs, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to muninn mcp: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func listRepos(t *testing.T, cs *sdk.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_repos", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("list_repos: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_repos returned a tool error: %+v", res.Content)
	}
}

func (e *e2e) daemonLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.state, "mcp.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPSessionsShareOneDaemon(t *testing.T) {
	e := newE2E(t)

	a := e.session(t)
	b := e.session(t)
	listRepos(t, a)
	listRepos(t, b)

	if got := strings.Count(e.daemonLog(t), "opened"); got != 2 {
		t.Errorf("daemon log has %d opened sessions, want 2:\n%s", got, e.daemonLog(t))
	}
	socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock"))
	if len(socks) != 1 {
		t.Errorf("socket files = %v, want exactly one", socks)
	}

	// With both sessions closed, the daemon idles out and removes its
	// socket.
	a.Close()
	b.Close()
	waitFor(t, "the daemon to idle out", func() bool {
		socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock"))
		return len(socks) == 0
	})
}

func TestMCPInProcessStartsNoDaemon(t *testing.T) {
	e := newE2E(t)

	listRepos(t, e.session(t, "--in-process"))

	if socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock")); len(socks) != 0 {
		t.Errorf("socket files = %v, want none for --in-process", socks)
	}
}
