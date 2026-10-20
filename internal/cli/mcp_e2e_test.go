package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// newE2E builds muninn, and muninn-mcp-proxy next to it when withProxy is
// set, the way a release installs them.
func newE2E(t *testing.T, withProxy bool) *e2e {
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
	if withProxy {
		build := exec.Command("go", "build", "-o", filepath.Join(root, "muninn-mcp-proxy"), "../../cmd/muninn-mcp-proxy")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build proxy: %v\n%s", err, out)
		}
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

// session starts `muninn mcp` with args the way an MCP client would, and
// returns the client session and the process serving it.
func (e *e2e) session(t *testing.T, args ...string) (*sdk.ClientSession, *exec.Cmd) {
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
	return cs, cmd
}

// program returns the base name of the program pid is running now, which
// changes when a process execs another binary.
func program(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	return filepath.Base(strings.TrimSpace(string(out)))
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
	tests := map[string]struct {
		withProxy bool
		program   string
	}{
		"forwarding from muninn":          {withProxy: false, program: "muninn"},
		"forwarding from the small proxy": {withProxy: true, program: "muninn-mcp-proxy"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := newE2E(t, tc.withProxy)

			a, cmd := e.session(t)
			b, _ := e.session(t)
			listRepos(t, a)
			listRepos(t, b)

			if got := program(t, cmd.Process.Pid); got != tc.program {
				t.Errorf("session runs %q, want %q", got, tc.program)
			}
			if got := strings.Count(e.daemonLog(t), "opened"); got != 2 {
				t.Errorf("daemon log has %d opened sessions, want 2:\n%s", got, e.daemonLog(t))
			}
			socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock"))
			if len(socks) != 1 {
				t.Errorf("socket files = %v, want exactly one", socks)
			}

			// With both sessions closed, the daemon idles out and removes
			// its socket.
			a.Close()
			b.Close()
			waitFor(t, "the daemon to idle out", func() bool {
				socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock"))
				return len(socks) == 0
			})
		})
	}
}

func TestMCPFallsBackInProcess(t *testing.T) {
	causes := map[string]func(t *testing.T, state string){
		// A file where the state directory should be: no daemon can start.
		"state dir is a file": func(t *testing.T, state string) {
			if err := os.WriteFile(state, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		// Other users could reach a socket in it, so the daemon refuses to
		// listen there.
		"state dir readable by others": func(t *testing.T, state string) {
			if err := os.Mkdir(state, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(state, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for cause, setup := range causes {
		for _, withProxy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/proxy=%v", cause, withProxy), func(t *testing.T) {
				e := newE2E(t, withProxy)
				if err := os.MkdirAll(filepath.Dir(e.state), 0o700); err != nil {
					t.Fatal(err)
				}
				setup(t, e.state)

				// A trailing "--" ends flag parsing, so falling back must
				// not rely on appending a flag to the original arguments.
				cs, cmd := e.session(t, "--")
				listRepos(t, cs)

				if got := program(t, cmd.Process.Pid); got != "muninn" {
					t.Errorf("session runs %q, want muninn serving in-process", got)
				}
				if socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock")); len(socks) != 0 {
					t.Errorf("socket files = %v, want none", socks)
				}
			})
		}
	}
}

func TestMCPInProcessStartsNoDaemon(t *testing.T) {
	e := newE2E(t, true)

	cs, _ := e.session(t, "--in-process")
	listRepos(t, cs)

	if socks, _ := filepath.Glob(filepath.Join(e.state, "mcp-*.sock")); len(socks) != 0 {
		t.Errorf("socket files = %v, want none for --in-process", socks)
	}
}
