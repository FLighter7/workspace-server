//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

var procFreeConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")

// detachConsole releases the console window a logon task opened for this
// console program. The window may flash for an instant before this runs. A
// process without a console gets no console events, so Ctrl+C and the
// window's close button are gone with it; the web page's Stop button and
// "workspace-server stop" are what stop it. The call fails, harmlessly, when
// there is no console to release, as when "start" launched the process
// without one.
func detachConsole() {
	_, _, _ = procFreeConsole.Call()
}

// stdinIsTerminal reports whether standard input is a console, where a
// person can answer a question. Go reports the null device (< NUL) as a
// character device too, so the console mode is what tells them apart.
func stdinIsTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
