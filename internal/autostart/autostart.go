// Package autostart registers the server to start at login: a LaunchAgent on
// macOS, a systemd user unit on Linux and a logon task on Windows. The
// builders of the registration files and command lines are pure and build on
// every OS; Install, Unregister and Uninstall live in one file per OS.
package autostart

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// launchdLabel names the LaunchAgent and the plist file.
	launchdLabel = "io.github.flighter7.workspace-server"
	// systemdUnitName is the file name of the systemd user unit.
	systemdUnitName = "workspace-server.service"
	// taskName names the Windows logon task.
	taskName = "workspace-server"
)

// Out receives the message Install prints about what it did.
var Out io.Writer = os.Stdout

// PageWait is how long OpenPage waits for a running server's web page to
// answer before it
// opens the browser anyway.
const PageWait = 10 * time.Second

// BinaryPath returns the path of the running executable with symbolic links
// resolved, the path the autostart entry must run.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}
	return resolved, nil
}

// LaunchdPlist returns the LaunchAgent plist that runs "<binary> run -dir
// <dir>" at login and starts it again after a crash. KeepAlive restarts it
// only after an unsuccessful exit: a stop the user asked for exits 0 and must
// stay stopped.
func LaunchdPlist(binary, dir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(binary) + `</string>
		<string>run</string>
		<string>-dir</string>
		<string>` + xmlEscape(dir) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
</dict>
</plist>
`
}

// SystemdUnit returns the systemd user unit that runs "<binary> run -dir
// <dir>" and restarts it on failure. The folder is named explicitly because
// the user manager's environment, XDG_DATA_HOME included, may differ from the
// shell's.
func SystemdUnit(binary, dir string) string {
	return `[Unit]
Description=Workspace server

[Service]
ExecStart=` + systemdExec(binary) + ` run -dir ` + systemdExec(dir) + `
Restart=on-failure

[Install]
WantedBy=default.target
`
}

// checkSystemdBinary refuses a program path systemd may not run as written:
// one holding "$". The path is escaped as "$$" in ExecStart, but
// systemd.service(5) does not promise to undo that in the executable, the
// first word, as it does in the arguments. A "%" is fine: specifiers are
// expanded there too, and "%%" is undone.
func checkSystemdBinary(binary string) error {
	if strings.Contains(binary, "$") {
		return fmt.Errorf("the program's path %s holds a \"$\", which systemd may not run as written; move the program to a folder without one and run start again", binary)
	}
	return nil
}

// systemdExec quotes a path for ExecStart. systemd expands %
// specifiers and $ variables in the command line, so both are doubled. A path
// with white space, quotes or backslashes goes in double quotes with those
// characters escaped.
func systemdExec(binary string) string {
	s := strings.NewReplacer("%", "%%", "$", "$$").Replace(binary)
	if !strings.ContainsAny(s, " \t\n\r\"'\\") {
		return s
	}
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
	return `"` + s + `"`
}

// SchtasksTaskXML returns the Task Scheduler 1.2 definition of the logon task
// that runs "<binary> run -background -dir <dir>" for user (DOMAIN\name). It is built for
// "schtasks /Create /XML", which a "/SC ONLOGON /TR" command line cannot
// replace: that form triggers on any user's logon, which a standard user may
// not create, and takes defaults that stop the server on battery and after 72
// hours. The document declares UTF-16 and must be written with
// utf16LEWithBOM. Every value is XML-escaped.
func SchtasksTaskXML(binary, dir, user string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlEscape(user) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(binary) + `</Command>
      <Arguments>` + xmlEscape("run -background -dir "+windowsArg(dir)) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// windowsArg quotes s as one argument of a Windows command line, by the rules
// CommandLineToArgvW and the Go runtime parse: quotes only when needed, a
// quote escaped with a backslash, and the backslashes before a quote, or
// before the closing quote, doubled.
func windowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(s[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// taskInList reports whether list, the output of "schtasks /Query /FO CSV
// /NH", names the task in the root folder. Each line starts with the task's
// path, so the logon task is \workspace-server. The text of schtasks' error
// messages is localized, which is why "no such task" is told from a failing
// query by listing every task, not by reading the message.
func taskInList(list []byte, name string) bool {
	r := csv.NewReader(bytes.NewReader(list))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err != nil { // io.EOF: the list is read to its end
			return false
		}
		if len(rec) > 0 && strings.EqualFold(rec[0], `\`+name) {
			return true
		}
	}
}

// utf16LEWithBOM encodes s as UTF-16 little endian with a byte order mark,
// the form Task Scheduler exports and "schtasks /XML" reads reliably.
func utf16LEWithBOM(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// xmlEscape escapes s for XML character data.
func xmlEscape(s string) string {
	var esc bytes.Buffer
	// EscapeText only fails when the writer does, and a Buffer never does.
	_ = xml.EscapeText(&esc, []byte(s))
	return esc.String()
}

// launchdPlistPath is where the LaunchAgent plist lives for the home
// directory home.
func launchdPlistPath(home string) string {
	return path.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// systemdUnitPath is where the user unit lives for the home directory home.
// It is fixed under ~/.config, not $XDG_CONFIG_HOME: the user manager that
// "systemctl --user" talks to searches with its own environment, not the
// shell's, so a path from the shell could be one it never looks in.
func systemdUnitPath(home string) string {
	return path.Join(home, ".config", "systemd", "user", systemdUnitName)
}

// commandLine renders a command for error messages.
func commandLine(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// runCommand runs name with args, without a shell. A failure is wrapped with
// the command line and the combined output.
func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s: %w", commandLine(name, args), err)
		}
		return fmt.Errorf("%s: %w: %s", commandLine(name, args), err, msg)
	}
	return nil
}

// step is one action of Unregister or Uninstall: a file to remove (a missing
// one is fine) or a command to run. A failing command is an error only when
// Must is set; otherwise it is best effort, such as unloading what may not
// be loaded.
type step struct {
	Remove string
	Cmd    []string
	Must   bool
}

// unregisterSteps lists what Unregister does on goos, in order. None of them
// stops a running server first: the server calling Unregister may be the one
// the registration started, and must finish its own exit. On macOS the
// bootout comes last for that reason (it ends the agent it unloads).
func unregisterSteps(goos, home string, uid int) []step {
	switch goos {
	case "darwin":
		return []step{
			{Remove: launchdPlistPath(home)},
			{Cmd: []string{"launchctl", "bootout", launchdTarget(uid)}},
		}
	case "linux":
		return []step{
			{Cmd: []string{"systemctl", "--user", "disable", systemdUnitName}},
			{Remove: systemdUnitPath(home)},
			{Cmd: []string{"systemctl", "--user", "daemon-reload"}},
		}
	case "windows":
		return []step{{Cmd: []string{"schtasks", "/Delete", "/TN", taskName, "/F"}, Must: true}}
	}
	return nil
}

// uninstallSteps lists what Uninstall does on goos, in order: stop the
// server through the OS, then remove the registration.
func uninstallSteps(goos, home string, uid int) []step {
	switch goos {
	case "darwin":
		return []step{
			{Cmd: []string{"launchctl", "bootout", launchdTarget(uid)}},
			{Remove: launchdPlistPath(home)},
		}
	case "linux":
		return []step{
			{Cmd: []string{"systemctl", "--user", "disable", "--now", systemdUnitName}, Must: true},
			{Remove: systemdUnitPath(home)},
			{Cmd: []string{"systemctl", "--user", "daemon-reload"}},
		}
	case "windows":
		return []step{
			{Cmd: []string{"schtasks", "/End", "/TN", taskName}},
			{Cmd: []string{"schtasks", "/Delete", "/TN", taskName, "/F"}, Must: true},
		}
	}
	return nil
}

// launchdTarget names the LaunchAgent in the user's GUI domain.
func launchdTarget(uid int) string {
	return "gui/" + strconv.Itoa(uid) + "/" + launchdLabel
}

// runSteps runs every step, even after a failure. did reports whether any
// step changed something: a file existed and was removed, or a command
// succeeded. err is the first failure of a Must step or of a removal.
func runSteps(steps []step, run func(name string, args ...string) error) (did bool, err error) {
	for _, s := range steps {
		var stepErr error
		if s.Remove != "" {
			switch e := os.Remove(s.Remove); {
			case e == nil:
				did = true
			case !errors.Is(e, os.ErrNotExist):
				stepErr = e
			}
		} else if e := run(s.Cmd[0], s.Cmd[1:]...); e == nil {
			did = true
		} else if s.Must {
			stepErr = e
		}
		if err == nil {
			err = stepErr
		}
	}
	return did, err
}

// PageURL is the address of the web page for httpPort.
func PageURL(httpPort int) string {
	return "http://127.0.0.1:" + strconv.Itoa(httpPort) + "/"
}

// OpenPage waits for the server to answer on httpPort for up to wait and
// then starts the browser on the page. A page that never answered is not
// opened: the browser would show an error. It does not wait for the
// browser: xdg-open may run one in the foreground. It reports whether the
// page answered and whether a browser was started; the caller tells the
// user.
func OpenPage(httpPort int, wait time.Duration) (answered, opened bool) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort))
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			answered = true
			break
		}
		if !time.Now().Before(deadline) {
			return false, false
		}
		time.Sleep(250 * time.Millisecond)
	}
	cmd := browserCommand(PageURL(httpPort))
	if cmd == nil || cmd.Start() != nil {
		return true, false
	}
	// Reap the opener when it exits, so it leaves no zombie behind.
	go func() { _ = cmd.Wait() }()
	return true, true
}
