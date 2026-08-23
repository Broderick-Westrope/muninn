// Package ctags locates and validates a universal-ctags binary suitable for
// Zoekt symbol extraction (requires the +interactive feature).
package ctags

import (
	"fmt"
	"os/exec"
	"strings"
)

const installHint = "install it with `brew install universal-ctags`"

// probeCandidates are tried in order when no path is configured. Absolute
// Homebrew paths are included because launchd's default PATH excludes them.
var probeCandidates = []string{
	"universal-ctags",
	"ctags",
	"/opt/homebrew/bin/ctags",
	"/usr/local/bin/ctags",
}

// Resolve returns an absolute path to a validated universal-ctags binary.
// If configured is non-empty it is validated directly; otherwise well-known
// names and locations are probed in order.
func Resolve(configured string) (string, error) {
	if configured != "" {
		if err := Validate(configured); err != nil {
			return "", fmt.Errorf("ctags path %q from config is invalid: %w", configured, err)
		}
		return configured, nil
	}

	for _, candidate := range probeCandidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if err := Validate(path); err != nil {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("no usable universal-ctags binary found; %s", installHint)
}

// Validate checks that the binary at path is universal-ctags with the
// interactive feature required by Zoekt.
func Validate(path string) error {
	version, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("running %s --version: %w; %s", path, err, installHint)
	}
	if !strings.Contains(string(version), "Universal Ctags") {
		return fmt.Errorf("%s is not Universal Ctags (--version check failed); %s", path, installHint)
	}

	features, err := exec.Command(path, "--list-features").Output()
	if err != nil {
		return fmt.Errorf("running %s --list-features: %w; %s", path, err, installHint)
	}
	if !hasFeature(string(features), "interactive") {
		return fmt.Errorf("%s lacks the interactive feature (--list-features check failed); %s", path, installHint)
	}
	return nil
}

func hasFeature(output, feature string) bool {
	for _, line := range strings.Split(output, "\n") {
		for _, word := range strings.Fields(line) {
			if word == feature {
				return true
			}
		}
	}
	return false
}
