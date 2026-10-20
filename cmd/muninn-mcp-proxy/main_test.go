package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The proxy exists to keep each session's forwarder small: linking Zoekt,
// or anything else outside the standard library, would quietly bring back
// tens of megabytes per session.
func TestProxyLinksOnlyTheStandardLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	allowed := map[string]bool{
		"github.com/broderick-westrope/muninn/cmd/muninn-mcp-proxy": true,
		"github.com/broderick-westrope/muninn/internal/mcpsock":     true,
	}
	for _, pkg := range strings.Fields(string(out)) {
		if !allowed[pkg] {
			t.Errorf("muninn-mcp-proxy links %s; it must use only the standard library and mcpsock", pkg)
		}
	}
}
