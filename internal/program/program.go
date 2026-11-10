// Package program manages the program file itself: its entry in Windows'
// list of installed programs (Settings → Apps), and its removal when the
// user deletes the server. Elsewhere there is no such list, and Register
// and Unregister do nothing.
package program

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// uninstallKey is the per-user uninstall entry's key under HKCU. Its name is
// fixed, so there is at most one entry: every start overwrites it.
const uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\workspace-server`

// ErrNotRegular is Remove's error for a path that is not a regular file,
// such as a folder or a symbolic link: Remove deletes nothing else.
var ErrNotRegular = errors.New("not a regular file")

// Value is one registry value of the uninstall entry: a string, or a DWORD
// when DWord is set.
type Value struct {
	Name   string
	String string
	DWord  *uint32
}

// Entry returns the values of the uninstall entry for the program file exe
// at version. Windows runs UninstallString in a console window; "delete
// --pause" asks before deleting and keeps the window open at the end.
func Entry(exe, version string) []Value {
	one := uint32(1)
	return []Value{
		{Name: "DisplayName", String: "workspace-server"},
		{Name: "DisplayVersion", String: version},
		{Name: "Publisher", String: "FLighter7"},
		{Name: "InstallLocation", String: windowsDir(exe)},
		{Name: "DisplayIcon", String: exe},
		{Name: "UninstallString", String: `"` + exe + `" delete --pause`},
		{Name: "NoModify", DWord: &one},
		{Name: "NoRepair", DWord: &one},
	}
}

// windowsDir returns the folder of a Windows path, built the same on every
// OS so that Entry can be tested anywhere.
func windowsDir(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		return path[:i]
	}
	return path
}

// helperScript is the PowerShell command that deletes path once the program
// that runs from it has exited: Windows does not let a running program
// delete its own file. It tries every 0.5 s, 20 times, which outlasts both
// the server and a delete command that run from the same file. The path is
// a single-quoted literal and is removed with -LiteralPath, so no character
// in it is interpreted. In such a literal PowerShell takes ' and the
// typographic single quotes ‘ ’ ‚ ‛ alike as the quote, and each is escaped
// by doubling it.
func helperScript(path string) string {
	quoted := "'" + singleQuotes.Replace(path) + "'"
	return "$p = " + quoted + "; " +
		"for ($i = 0; $i -lt 20; $i++) { " +
		"try { Remove-Item -LiteralPath $p -Force -ErrorAction Stop; exit 0 } " +
		"catch { if (-not (Test-Path -LiteralPath $p)) { exit 0 } }; " +
		"Start-Sleep -Milliseconds 500 }; exit 1"
}

var singleQuotes = strings.NewReplacer(
	"'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019", "\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b")

// checkRegular returns nil when path is a regular file, not following a
// symbolic link. A missing file wraps os.ErrNotExist; anything else that is
// not a regular file is ErrNotRegular.
func checkRegular(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the program file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("the program file %s: %w", path, ErrNotRegular)
	}
	return nil
}
