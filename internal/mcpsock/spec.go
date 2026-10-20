package mcpsock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// InProcessEnv, set to any non-empty value, makes `muninn mcp` serve
// in-process. The proxy sets it when it hands a session back to muninn:
// an environment variable survives any argument list, where a flag
// appended after a "--" would be read as a positional argument.
const InProcessEnv = "MUNINN_MCP_IN_PROCESS"

// DaemonStartFlag and SpecFlag are the `muninn mcp` flags that start the
// shared MCP server for a Spec. Connect runs them and muninn registers
// them, so this is their one definition.
const (
	DaemonStartFlag = "daemon-start"
	SpecFlag        = "spec"
)

// maxSocketPath is the longest socket path macOS accepts: sun_path holds
// 104 bytes including the terminating NUL.
const maxSocketPath = 103

// ErrSocketPathTooLong reports a state directory so deep that the socket
// path doesn't fit in sun_path. The caller should serve in-process.
var ErrSocketPathTooLong = errors.New("socket path too long")

// Spec is everything a session resolved from its own environment, fixed
// once by the forwarder and handed to the daemon so the daemon never
// re-resolves anything from its own environment or working directory. All
// paths are absolute.
type Spec struct {
	// Muninn is the resolved path of the muninn binary that serves.
	Muninn     string `json:"muninn"`
	IndexDir   string `json:"index_dir"`
	StatusPath string `json:"status_path"`
	MirrorsDir string `json:"mirrors_dir"`
	StateDir   string `json:"state_dir"`
	// Identity names the daemon this session may share: sessions share a
	// daemon only if they resolved the same binary and the same paths.
	Identity string `json:"identity"`

	// encoded is the text a decoded Spec came from. Encode returns it
	// unchanged, so a proxy older than the muninn that wrote it passes on
	// fields it doesn't know.
	encoded string
}

// NewSpec resolves a Spec. muninn is resolved through symlinks (Homebrew
// installs a symlink into a versioned Cellar path) and its size and
// modification time join the identity, so an upgrade or a rebuild gets a
// fresh daemon. That is a heuristic: a replacement that keeps the path,
// size and mtime would share the old daemon.
func NewSpec(muninn, indexDir, statusPath, mirrorsDir, stateDir string) (Spec, error) {
	bin, err := filepath.EvalSymlinks(muninn)
	if err != nil {
		return Spec{}, fmt.Errorf("resolving muninn binary %s: %w", muninn, err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return Spec{}, fmt.Errorf("resolving muninn binary %s: %w", muninn, err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		return Spec{}, fmt.Errorf("reading muninn binary %s: %w", bin, err)
	}
	s := Spec{Muninn: bin}
	for _, p := range []struct {
		dst *string
		src string
	}{
		{&s.IndexDir, indexDir},
		{&s.StatusPath, statusPath},
		{&s.MirrorsDir, mirrorsDir},
		{&s.StateDir, stateDir},
	} {
		if *p.dst, err = filepath.Abs(p.src); err != nil {
			return Spec{}, fmt.Errorf("resolving %s: %w", p.src, err)
		}
	}

	h := sha256.New()
	for _, part := range []string{
		bin, strconv.FormatInt(info.Size(), 10), strconv.FormatInt(info.ModTime().UnixNano(), 10),
		s.IndexDir, s.StatusPath, s.MirrorsDir, s.StateDir,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	s.Identity = hex.EncodeToString(h.Sum(nil))[:16]

	if len(s.Socket()) > maxSocketPath {
		return Spec{}, fmt.Errorf("%w: %s", ErrSocketPathTooLong, s.Socket())
	}
	return s, nil
}

// Socket is where the daemon for this identity listens.
func (s Spec) Socket() string { return s.stateFile(".sock") }

// Lock is held by the daemon for its whole life, so a free lock means a
// socket file left behind is stale.
func (s Spec) Lock() string { return s.stateFile(".lock") }

// Log is where the daemon writes, since it has no client to show stderr.
func (s Spec) Log() string { return filepath.Join(s.StateDir, "mcp.log") }

func (s Spec) stateFile(ext string) string {
	return filepath.Join(s.StateDir, "mcp-"+s.Identity+ext)
}

// Encode renders s for passing to another process as one argument.
func (s Spec) Encode() string {
	if s.encoded != "" {
		return s.encoded
	}
	b, _ := json.Marshal(s) // only strings; can't fail
	return string(b)
}

// DecodeSpec parses a Spec produced by Encode.
func DecodeSpec(encoded string) (Spec, error) {
	var s Spec
	if err := json.Unmarshal([]byte(encoded), &s); err != nil {
		return Spec{}, fmt.Errorf("decoding spec: %w", err)
	}
	for name, v := range map[string]string{
		"muninn": s.Muninn, "index_dir": s.IndexDir, "status_path": s.StatusPath,
		"mirrors_dir": s.MirrorsDir, "state_dir": s.StateDir, "identity": s.Identity,
	} {
		if v == "" {
			return Spec{}, fmt.Errorf("decoding spec: %s is missing", name)
		}
	}
	s.encoded = encoded
	return s, nil
}
