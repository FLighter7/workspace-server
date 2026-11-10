// Package datadir finds the server folder: where the server keeps its
// settings, identity, registries, log and lock, and, unless config.json names
// another data folder, the apps' data.
package datadir

import (
	"fmt"
	"os"
	"path"
	"runtime"
)

const dirName = "workspace-server"

// Default returns the default server folder for the current operating
// system.
func Default() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}

	return defaultFor(runtime.GOOS, home, os.Getenv), nil
}

// defaultFor is the pure part of Default: macOS keeps the directory in
// Application Support, Windows in %APPDATA%, other systems in
// $XDG_DATA_HOME. It joins with the separator of goos, not of the host.
func defaultFor(goos, home string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return path.Join(home, "Library", "Application Support", dirName)
	case "windows":
		base := getenv("APPDATA")
		if base == "" {
			base = home + `\AppData\Roaming`
		}

		return base + `\` + dirName
	default:
		base := getenv("XDG_DATA_HOME")
		if base == "" {
			base = path.Join(home, ".local", "share")
		}

		return path.Join(base, dirName)
	}
}
