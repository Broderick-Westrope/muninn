// Package launchd installs and removes the scheduled sync job as a macOS
// launchd user agent, with everything (binary path, config path, schedule)
// baked into the plist at install time.
package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// Label identifies the launchd job.
const Label = "dev.broderick-westrope.muninn"

const defaultIntervalMinutes = 60

// PlistPath returns the agent's plist path in ~/Library/LaunchAgents.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Launchctl abstracts launchctl invocations so tests can stub them.
type Launchctl interface {
	// Bootout unloads the job from the user's GUI domain. It returns an
	// error even when the job is simply not loaded; callers ignore it for
	// idempotency.
	Bootout(label string) error
	// Bootstrap loads the plist into the user's GUI domain.
	Bootstrap(plistPath string) error
}

// ExecLaunchctl runs the real launchctl binary against gui/<uid>.
type ExecLaunchctl struct{}

func (ExecLaunchctl) Bootout(label string) error {
	return runLaunchctl("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
}

func (ExecLaunchctl) Bootstrap(plistPath string) error {
	return runLaunchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
}

func runLaunchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BinaryPath}}</string>
		<string>sync</string>
		<string>--config</string>
		<string>{{.ConfigPath}}</string>
	</array>
	<key>StartInterval</key>
	<integer>{{.StartInterval}}</integer>
	<key>RunAtLoad</key>
	<false/>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`))

type plistData struct {
	Label         string
	BinaryPath    string
	ConfigPath    string
	StartInterval int
	LogPath       string
}

// Render produces the plist for the sync agent. binaryPath and configPath
// must be absolute (launchd provides no working directory or shell env).
// intervalMinutes <= 0 falls back to hourly.
func Render(binaryPath, configPath, logPath string, intervalMinutes int) ([]byte, error) {
	if intervalMinutes <= 0 {
		intervalMinutes = defaultIntervalMinutes
	}
	var buf bytes.Buffer
	err := plistTemplate.Execute(&buf, plistData{
		Label:         Label,
		BinaryPath:    binaryPath,
		ConfigPath:    configPath,
		StartInterval: intervalMinutes * 60,
		LogPath:       logPath,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering plist: %w", err)
	}
	return buf.Bytes(), nil
}

// Install writes the plist and (re)loads the job. The job is booted out
// first because a bare bootstrap errors when it is already loaded; the
// bootout error is ignored so install is idempotent.
func Install(lc Launchctl, plistPath string, plist []byte) error {
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("creating LaunchAgents directory: %w", err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("writing plist %s: %w", plistPath, err)
	}
	_ = lc.Bootout(Label) // errors when the job is not loaded; ignored
	if err := lc.Bootstrap(plistPath); err != nil {
		return fmt.Errorf("loading launchd agent: %w", err)
	}
	return nil
}

// Uninstall boots the job out and removes the plist; idempotent.
func Uninstall(lc Launchctl, plistPath string) error {
	_ = lc.Bootout(Label) // errors when the job is not loaded; ignored
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing plist %s: %w", plistPath, err)
	}
	return nil
}
