package ctags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCtags writes an executable shell script named name into dir that
// responds to --version with version and to --list-features with features.
// Output uses only shell builtins so the script works with an empty PATH.
func writeFakeCtags(t *testing.T, dir, name, version, features string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	echoLines := func(s string) string {
		var b strings.Builder
		for _, line := range strings.Split(s, "\n") {
			b.WriteString("echo '" + line + "'\n")
		}
		return b.String()
	}
	script := "#!/bin/sh\ncase \"$1\" in\n--version)\n" + echoLines(version) +
		";;\n--list-features)\n" + echoLines(features) + ";;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	universalVersion = "Universal Ctags 6.1.0, Copyright (C) 2015-2023 Universal Ctags Team"
	bsdVersion       = "ctags (BSD) 1.0"
	fullFeatures     = "wildcards\nregex\ninteractive\njson\nyaml"
	noInteractive    = "wildcards\nregex\njson\nyaml"
)

func TestValidate(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid universal ctags", func(t *testing.T) {
		path := writeFakeCtags(t, dir, "good-ctags", universalVersion, fullFeatures)
		if err := Validate(path); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("bsd ctags rejected", func(t *testing.T) {
		path := writeFakeCtags(t, dir, "bsd-ctags", bsdVersion, fullFeatures)
		err := Validate(path)
		if err == nil {
			t.Fatal("Validate succeeded, want error")
		}
		if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "Universal Ctags") {
			t.Errorf("error %q should name the path and the version check", err)
		}
		if !strings.Contains(err.Error(), "brew install universal-ctags") {
			t.Errorf("error %q should suggest brew install", err)
		}
	})

	t.Run("missing interactive feature rejected", func(t *testing.T) {
		path := writeFakeCtags(t, dir, "no-interactive-ctags", universalVersion, noInteractive)
		err := Validate(path)
		if err == nil {
			t.Fatal("Validate succeeded, want error")
		}
		if !strings.Contains(err.Error(), "interactive") || !strings.Contains(err.Error(), path) {
			t.Errorf("error %q should mention interactive and the path", err)
		}
	})

	t.Run("nonexistent binary", func(t *testing.T) {
		if err := Validate(filepath.Join(dir, "does-not-exist")); err == nil {
			t.Error("Validate succeeded, want error")
		}
	})
}

func TestResolveConfigured(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid configured path", func(t *testing.T) {
		path := writeFakeCtags(t, dir, "configured-ctags", universalVersion, fullFeatures)
		got, err := Resolve(path)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != path {
			t.Errorf("Resolve = %q, want %q", got, path)
		}
	})

	t.Run("invalid configured path mentions config", func(t *testing.T) {
		path := writeFakeCtags(t, dir, "configured-bsd", bsdVersion, fullFeatures)
		_, err := Resolve(path)
		if err == nil {
			t.Fatal("Resolve succeeded, want error")
		}
		if !strings.Contains(err.Error(), "config") {
			t.Errorf("error %q should mention the path came from config", err)
		}
	})
}

func TestResolveProbeOrder(t *testing.T) {
	t.Run("universal-ctags preferred over ctags", func(t *testing.T) {
		dir := t.TempDir()
		preferred := writeFakeCtags(t, dir, "universal-ctags", universalVersion, fullFeatures)
		writeFakeCtags(t, dir, "ctags", universalVersion, fullFeatures)
		t.Setenv("PATH", dir)
		got, err := Resolve("")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != preferred {
			t.Errorf("Resolve = %q, want %q", got, preferred)
		}
	})

	t.Run("falls through invalid candidates", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeCtags(t, dir, "universal-ctags", bsdVersion, fullFeatures)
		valid := writeFakeCtags(t, dir, "ctags", universalVersion, fullFeatures)
		t.Setenv("PATH", dir)
		got, err := Resolve("")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != valid {
			t.Errorf("Resolve = %q, want %q", got, valid)
		}
	})

	t.Run("nothing usable on PATH", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeCtags(t, dir, "ctags", bsdVersion, fullFeatures)
		t.Setenv("PATH", dir)
		if _, err := os.Stat("/opt/homebrew/bin/ctags"); err == nil {
			t.Skip("real /opt/homebrew/bin/ctags present")
		}
		if _, err := os.Stat("/usr/local/bin/ctags"); err == nil {
			t.Skip("real /usr/local/bin/ctags present")
		}
		_, err := Resolve("")
		if err == nil {
			t.Fatal("Resolve succeeded, want error")
		}
		if !strings.Contains(err.Error(), "brew install universal-ctags") {
			t.Errorf("error %q should suggest brew install", err)
		}
	})
}
