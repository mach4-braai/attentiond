package herdr

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveSocket finds the Herdr control socket, following Herdr's documented
// resolution order: an explicit override, HERDR_SOCKET_PATH, then the socket
// for HERDR_SESSION, then the default session socket.
//
// Herdr documents the config directory as ~/.config/herdr on Unix. Some
// releases follow the platform convention on macOS instead, so that location is
// also probed rather than making the daemon unusable on a working install.
func ResolveSocket(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("herdr socket %s: %w", explicit, err)
		}
		return explicit, nil
	}
	if fromEnv := os.Getenv("HERDR_SOCKET_PATH"); fromEnv != "" {
		if _, err := os.Stat(fromEnv); err != nil {
			return "", fmt.Errorf("HERDR_SOCKET_PATH %s: %w", fromEnv, err)
		}
		return fromEnv, nil
	}

	candidates := socketCandidates(os.Getenv("HERDR_SESSION"))
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no herdr socket found, looked in %s", strings.Join(candidates, ", "))
}

func socketCandidates(session string) []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "herdr"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "herdr"))
		if runtime.GOOS == "darwin" {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "herdr"))
		}
	}

	seen := make(map[string]bool, len(dirs))
	candidates := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		path := filepath.Join(dir, "herdr.sock")
		if session != "" {
			path = filepath.Join(dir, "sessions", session, "herdr.sock")
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		candidates = append(candidates, path)
	}
	return candidates
}
