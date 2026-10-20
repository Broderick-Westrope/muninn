// Command muninn-mcp-proxy carries one MCP session between a client's
// stdio and muninn's shared MCP daemon. `muninn mcp` execs it; it isn't
// meant to be run by hand.
//
// It exists because of memory: muninn links Zoekt, whose package init
// alone costs about 26 MB, and every MCP session keeps one forwarder alive
// for its whole life. This binary links only the standard library.
//
// Usage: muninn-mcp-proxy --spec <spec> -- <muninn arguments>
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/broderick-westrope/muninn/internal/mcpsock"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 3 || args[0] != "--"+mcpsock.SpecFlag || args[2] != "--" {
		fmt.Fprintln(os.Stderr, "usage: muninn-mcp-proxy --spec <spec> -- <muninn arguments>")
		return 2
	}
	spec, err := mcpsock.DecodeSpec(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "muninn-mcp-proxy: %v\n", err)
		return 2
	}
	muninnArgs := args[3:]

	conn, err := mcpsock.Connect(context.Background(), spec)
	if err != nil {
		// Nothing has been read from stdin, so muninn can still serve this
		// session itself.
		fmt.Fprintf(os.Stderr, "muninn: serving in-process: %v\n", err)
		argv := append([]string{spec.Muninn}, muninnArgs...)
		err := syscall.Exec(spec.Muninn, argv, inProcessEnv(os.Environ()))
		fmt.Fprintf(os.Stderr, "muninn-mcp-proxy: running %s: %v\n", spec.Muninn, err)
		return 1
	}
	defer conn.Close()
	if err := mcpsock.Forward(conn, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "muninn: %v\n", err)
		return 1
	}
	return 0
}

// inProcessEnv returns env with mcpsock.InProcessEnv set. Any inherited
// value is dropped rather than shadowed: with duplicates, the first wins,
// and an inherited empty value would send muninn straight back here.
func inProcessEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, mcpsock.InProcessEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, mcpsock.InProcessEnv+"=1")
}
