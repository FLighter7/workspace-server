package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// parsePlist decodes the plist into a flat list of the tokens that matter:
// every <key>, <string> and the empty elements <true/>, <false/>, in order.
func parsePlist(t *testing.T, doc string) []string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	var out []string
	var current string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, doc)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			current = tok.Name.Local
			if current == "true" || current == "false" {
				out = append(out, "<"+current+"/>")
			}
		case xml.CharData:
			if current == "key" || current == "string" {
				out = append(out, current+":"+string(tok))
			}
		case xml.EndElement:
			current = ""
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	doc := LaunchdPlist("/opt/ws/workspace-server", "/Users/a/Library/Application Support/workspace-server")
	tokens := parsePlist(t, doc)
	for _, want := range []string{
		"key:Label", "string:io.github.flighter7.workspace-server",
		"key:ProgramArguments", "string:/opt/ws/workspace-server", "string:run",
		"string:-dir", "string:/Users/a/Library/Application Support/workspace-server",
		"key:RunAtLoad", "key:KeepAlive", "key:SuccessfulExit",
	} {
		if !slices.Contains(tokens, want) {
			t.Errorf("plist lacks %q:\n%s", want, doc)
		}
	}
	// RunAtLoad true; KeepAlive is a dict that restarts only after a failed
	// exit, so a requested stop (exit 0) stays stopped.
	idx := func(want string) int {
		for i, tok := range tokens {
			if tok == want {
				return i
			}
		}
		return -1
	}
	if i := idx("key:RunAtLoad"); i < 0 || i+1 >= len(tokens) || tokens[i+1] != "<true/>" {
		t.Errorf("RunAtLoad must be true:\n%s", doc)
	}
	if i := idx("key:SuccessfulExit"); i < 0 || i+1 >= len(tokens) || tokens[i+1] != "<false/>" {
		t.Errorf("KeepAlive must hold SuccessfulExit false:\n%s", doc)
	}
	if i := idx("key:KeepAlive"); i < 0 || tokens[i+1] != "key:SuccessfulExit" {
		t.Errorf("KeepAlive must be a dict, not a bool:\n%s", doc)
	}
	// ProgramArguments are the binary, "run", "-dir", the folder, in order.
	bin := idx("string:/opt/ws/workspace-server")
	if bin < 0 || bin+3 >= len(tokens) ||
		tokens[bin+1] != "string:run" || tokens[bin+2] != "string:-dir" ||
		tokens[bin+3] != "string:/Users/a/Library/Application Support/workspace-server" {
		t.Errorf("ProgramArguments must be [binary, run, -dir, folder]:\n%s", doc)
	}
}

func TestLaunchdPlistEscapesThePath(t *testing.T) {
	path := `/Users/a & b/<ws> "x"/workspace-server`
	dir := `/Users/a & b/<data> "y"`
	doc := LaunchdPlist(path, dir)
	tokens := parsePlist(t, doc)
	if !slices.Contains(tokens, "string:"+path) || !slices.Contains(tokens, "string:"+dir) {
		t.Errorf("the paths did not survive the XML round trip:\n%s", doc)
	}
}

func TestSystemdUnit(t *testing.T) {
	unit := SystemdUnit("/opt/ws/workspace-server", "/home/a/.local/share/workspace-server")
	for _, want := range []string{
		"ExecStart=/opt/ws/workspace-server run -dir /home/a/.local/share/workspace-server\n",
		"Restart=on-failure\n",
		"WantedBy=default.target\n",
		"[Unit]", "[Service]", "[Install]",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

func TestSystemdUnitQuotesTheFolder(t *testing.T) {
	unit := SystemdUnit("/opt/ws", "/home/a b/$x%y")
	if want := `ExecStart=/opt/ws run -dir "/home/a b/$$x%%y"` + "\n"; !strings.Contains(unit, want) {
		t.Errorf("want line %q in:\n%s", want, unit)
	}
}

func TestSystemdUnitQuotesTheExecutable(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/opt/my apps/workspace-server", `ExecStart="/opt/my apps/workspace-server" run`},
		{`/opt/a"b/workspace-server`, `ExecStart="/opt/a\"b/workspace-server" run`},
		{`/opt/a\b/workspace-server`, `ExecStart="/opt/a\\b/workspace-server" run`},
		{"/opt/50%/workspace-server", "ExecStart=/opt/50%%/workspace-server run"},
		{"/opt/it's/ws", `ExecStart="/opt/it's/ws" run`},
		{"/opt/a b%c/ws", `ExecStart="/opt/a b%%c/ws" run`},
	}
	for _, c := range cases {
		unit := SystemdUnit(c.path, "/d")
		if !strings.Contains(unit, c.want+" -dir /d\n") {
			t.Errorf("path %q: want line %q in:\n%s", c.path, c.want, unit)
		}
	}
}

// "$" is escaped in the arguments, where systemd undoes "$$"; a program path
// holding one is refused, since systemd may not undo it there.
func TestSystemdDollar(t *testing.T) {
	unit := SystemdUnit("/opt/ws", "/home/a/$HOME data")
	if !strings.Contains(unit, `ExecStart=/opt/ws run -dir "/home/a/$$HOME data"`+"\n") {
		t.Errorf("the folder's $ is not escaped:\n%s", unit)
	}
	if err := checkSystemdBinary("/opt/$HOME/workspace-server"); err == nil || !strings.Contains(err.Error(), "/opt/$HOME/workspace-server") {
		t.Errorf("checkSystemdBinary with $ = %v, want an error naming the path", err)
	}
	if err := checkSystemdBinary("/opt/50% off/ws"); err != nil {
		t.Errorf("checkSystemdBinary without $ = %v", err)
	}
}

// passthroughCharset lets encoding/xml read a document that declares UTF-16
// although the test holds it as a Go string.
func passthroughCharset(_ string, in io.Reader) (io.Reader, error) { return in, nil }

// taskXMLValues returns the text of every element of the task XML, keyed by
// its path, joined by "/" (the last value wins when a path repeats).
func taskXMLValues(t *testing.T, doc string) map[string]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.CharsetReader = passthroughCharset
	values := map[string]string{}
	var stack []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return values
		}
		if err != nil {
			t.Fatalf("task XML is not well-formed: %v\n%s", err, doc)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			stack = append(stack, tok.Name.Local)
			if tok.Name.Local == "Task" {
				for _, a := range tok.Attr {
					values["Task@"+a.Name.Local] = a.Value
				}
				if tok.Name.Space != "http://schemas.microsoft.com/windows/2004/02/mit/task" {
					t.Errorf("Task namespace %q", tok.Name.Space)
				}
			}
			for _, a := range tok.Attr {
				values[strings.Join(stack, "/")+"@"+a.Name.Local] = a.Value
			}
		case xml.CharData:
			if s := strings.TrimSpace(string(tok)); s != "" {
				values[strings.Join(stack, "/")] = s
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
}

func TestSchtasksTaskXML(t *testing.T) {
	const user = `DESKTOP-1\alex`
	doc := SchtasksTaskXML(`C:\Program Files\Workspace & Co\workspace-server.exe`, `C:\Users\a b\AppData\Roaming\workspace-server`, user)
	if !strings.HasPrefix(doc, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("missing the UTF-16 declaration:\n%s", doc)
	}
	v := taskXMLValues(t, doc)
	want := map[string]string{
		"Task@version":                             "1.2",
		"Task/Triggers/LogonTrigger/Enabled":       "true",
		"Task/Triggers/LogonTrigger/UserId":        user,
		"Task/Principals/Principal@id":             "Author",
		"Task/Principals/Principal/UserId":         user,
		"Task/Principals/Principal/LogonType":      "InteractiveToken",
		"Task/Principals/Principal/RunLevel":       "LeastPrivilege",
		"Task/Settings/MultipleInstancesPolicy":    "IgnoreNew",
		"Task/Settings/DisallowStartIfOnBatteries": "false",
		"Task/Settings/StopIfGoingOnBatteries":     "false",
		"Task/Settings/ExecutionTimeLimit":         "PT0S",
		"Task/Settings/AllowStartOnDemand":         "true",
		"Task/Settings/Enabled":                    "true",
		"Task/Actions@Context":                     "Author",
		"Task/Actions/Exec/Command":                `C:\Program Files\Workspace & Co\workspace-server.exe`,
		"Task/Actions/Exec/Arguments":              `run -background -dir "C:\Users\a b\AppData\Roaming\workspace-server"`,
	}
	for k, w := range want {
		if got, ok := v[k]; !ok || got != w {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, w)
		}
	}
}

// taskXMLChildren returns, for each element path, the names of its child
// elements in document order.
func taskXMLChildren(t *testing.T, doc string) map[string][]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.CharsetReader = passthroughCharset
	children := map[string][]string{}
	var stack []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return children
		}
		if err != nil {
			t.Fatalf("task XML is not well-formed: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 {
				parent := strings.Join(stack, "/")
				children[parent] = append(children[parent], tok.Name.Local)
			}
			stack = append(stack, tok.Name.Local)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
}

// Task Scheduler reads the elements in its schema's order; Settings follows
// the order of its own export.
func TestSchtasksTaskXMLElementOrder(t *testing.T) {
	c := taskXMLChildren(t, SchtasksTaskXML(`C:\ws.exe`, `C:\d`, `D\u`))
	for path, want := range map[string][]string{
		"Task": {"Triggers", "Principals", "Settings", "Actions"},
		"Task/Settings": {"MultipleInstancesPolicy", "DisallowStartIfOnBatteries", "StopIfGoingOnBatteries",
			"AllowStartOnDemand", "Enabled", "ExecutionTimeLimit"},
		"Task/Principals/Principal": {"UserId", "LogonType", "RunLevel"},
		"Task/Actions/Exec":         {"Command", "Arguments"},
	} {
		if got := c[path]; !slices.Equal(got, want) {
			t.Errorf("%s children %v, want %v", path, got, want)
		}
	}
}

// TestRunCommandHelper is the program runCommand runs in the tests below;
// it does nothing unless WS_RUNCMD names a mode.
func TestRunCommandHelper(t *testing.T) {
	switch os.Getenv("WS_RUNCMD") {
	case "out":
		fmt.Print("boom\n")
		os.Exit(3)
	case "silent":
		os.Exit(4)
	case "ok":
		os.Exit(0)
	}
}

func TestRunCommandWrapsFailures(t *testing.T) {
	self := os.Args[0]
	args := []string{"-test.run=^TestRunCommandHelper$"}
	line := commandLine(self, args)

	t.Setenv("WS_RUNCMD", "out")
	err := runCommand(self, args...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	if want := line + ": exit status 3: boom"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}

	t.Setenv("WS_RUNCMD", "silent")
	if err := runCommand(self, args...); err == nil || err.Error() != line+": exit status 4" {
		t.Errorf("silent failure: %v, want the command line and status only", err)
	}

	t.Setenv("WS_RUNCMD", "ok")
	if err := runCommand(self, args...); err != nil {
		t.Errorf("success: %v", err)
	}

	if err := runCommand("no-such-program-ws-7f3a"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("missing program: %v, want exec.ErrNotFound", err)
	}
}

func TestSchtasksTaskXMLEscapesTheUser(t *testing.T) {
	const user = `D<O>&M\a"b`
	v := taskXMLValues(t, SchtasksTaskXML(`C:\ws.exe`, `C:\d`, user))
	if got := v["Task/Principals/Principal/UserId"]; got != user {
		t.Errorf("user %q did not round-trip, got %q", user, got)
	}
}

func TestUTF16LEWithBOM(t *testing.T) {
	const s = "<UserId>Алекс\\日本 \U0001F600</UserId>"
	b := utf16LEWithBOM(s)
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("no BOM: % x", b[:min(len(b), 4)])
	}
	if (len(b)-2)%2 != 0 {
		t.Fatalf("odd length %d", len(b))
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 2; i < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != s {
		t.Errorf("round trip %q, want %q", got, s)
	}
}

func TestPaths(t *testing.T) {
	if got, want := launchdPlistPath("/Users/a"), "/Users/a/Library/LaunchAgents/io.github.flighter7.workspace-server.plist"; got != want {
		t.Errorf("plist path %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "/cfg") // the unit path must not follow it
	if got, want := systemdUnitPath("/home/a"), "/home/a/.config/systemd/user/workspace-server.service"; got != want {
		t.Errorf("unit path %q, want %q", got, want)
	}
}

func TestWindowsArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\data`, `C:\data`},
		{`C:\a b\data`, `"C:\a b\data"`},
		{`C:\a"b`, `"C:\a\"b"`},
		{`C:\a b\`, `"C:\a b\\"`},
		{``, `""`},
	}
	for _, c := range cases {
		if got := windowsArg(c.in); got != c.want {
			t.Errorf("windowsArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSchtasksTaskXMLFolderWithoutSpaces(t *testing.T) {
	v := taskXMLValues(t, SchtasksTaskXML(`C:\ws.exe`, `C:\d`, `u`))
	if got, want := v["Task/Actions/Exec/Arguments"], `run -background -dir C:\d`; got != want {
		t.Errorf("Arguments %q, want %q", got, want)
	}
}

// stepStrings renders steps for comparison: a file removal as "rm <path>",
// a command as its words.
func stepStrings(steps []step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		if s.Remove != "" {
			out[i] = "rm " + s.Remove
		} else {
			out[i] = strings.Join(s.Cmd, " ")
		}
	}
	return out
}

func TestUnregisterNeverStopsTheCaller(t *testing.T) {
	cases := map[string][]string{
		"darwin": {
			"rm /Users/a/Library/LaunchAgents/io.github.flighter7.workspace-server.plist",
			"launchctl bootout gui/501/io.github.flighter7.workspace-server",
		},
		"linux": {
			"systemctl --user disable workspace-server.service",
			"rm /home/a/.config/systemd/user/workspace-server.service",
			"systemctl --user daemon-reload",
		},
		"windows": {"schtasks /Delete /TN workspace-server /F"},
	}
	homes := map[string]string{"darwin": "/Users/a", "linux": "/home/a", "windows": `C:\Users\a`}
	for goos, want := range cases {
		got := stepStrings(unregisterSteps(goos, homes[goos], 501))
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: steps\n%s\nwant\n%s", goos, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, s := range got {
			if strings.Contains(s, "/End") || strings.Contains(s, "--now") {
				t.Errorf("%s: %q would stop the calling server", goos, s)
			}
		}
	}
}

func TestUninstallStopsTheServer(t *testing.T) {
	cases := map[string][]string{
		"darwin": {
			"launchctl bootout gui/501/io.github.flighter7.workspace-server",
			"rm /Users/a/Library/LaunchAgents/io.github.flighter7.workspace-server.plist",
		},
		"linux": {
			"systemctl --user disable --now workspace-server.service",
			"rm /home/a/.config/systemd/user/workspace-server.service",
			"systemctl --user daemon-reload",
		},
		"windows": {"schtasks /End /TN workspace-server", "schtasks /Delete /TN workspace-server /F"},
	}
	homes := map[string]string{"darwin": "/Users/a", "linux": "/home/a", "windows": `C:\Users\a`}
	for goos, want := range cases {
		got := stepStrings(uninstallSteps(goos, homes[goos], 501))
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: steps\n%s\nwant\n%s", goos, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

func TestRunStepsAttemptsEveryStep(t *testing.T) {
	dir := t.TempDir()
	present := dir + "/present"
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var ran []string
	run := func(name string, args ...string) error {
		ran = append(ran, name)
		if name == "fail" {
			return errors.New("boom")
		}
		return nil
	}
	did, err := runSteps([]step{
		{Cmd: []string{"fail"}, Must: true},
		{Remove: dir + "/missing"},
		{Remove: present},
		{Cmd: []string{"ok"}},
	}, run)
	if !did {
		t.Error("did = false; a file was removed and a command succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the failing Must step's error", err)
	}
	if strings.Join(ran, ",") != "fail,ok" {
		t.Errorf("ran %v, want every command attempted", ran)
	}
	if _, statErr := os.Stat(present); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the present file was not removed")
	}
}

func TestRunStepsBestEffortFailureIsNoError(t *testing.T) {
	did, err := runSteps([]step{{Cmd: []string{"x"}}}, func(string, ...string) error { return errors.New("not loaded") })
	if did || err != nil {
		t.Errorf("did %v err %v, want false and nil", did, err)
	}
}

func TestTaskInList(t *testing.T) {
	// What "schtasks /Query /FO CSV /NH" prints: one line per task, starting
	// with its path. The names around ours are real ones from a Windows 10 box.
	line := func(path, status string) string { return `"` + path + `","N/A","` + status + `"` + "\r\n" }
	list := line(`\CreateExplorerShellUnelevatedTask`, "Ready") +
		line(`\workspace-server`, "Running") +
		line(`\StartIsBack health check`, "Ready") +
		line(`\Microsoft\Windows\Defrag\ScheduledDefrag`, "Ready")
	for _, c := range []struct {
		name, list string
		want       bool
	}{
		{"listed", list, true},
		{"listed in other case", strings.ToUpper(list), true},
		{"not listed", strings.Replace(list, "workspace-server", "other-server", 1), false},
		{"only in a subfolder", line(`\Vendor\workspace-server`, "Ready"), false},
		{"only a prefix", line(`\workspace-server-old`, "Ready"), false},
		{"only in another column", `"\other","\workspace-server","Ready"` + "\r\n", false},
		{"empty", "", false},
		{"no trailing newline", strings.TrimSuffix(line(`\workspace-server`, "Ready"), "\r\n"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := taskInList([]byte(c.list), "workspace-server"); got != c.want {
				t.Errorf("taskInList = %v, want %v for\n%s", got, c.want, c.list)
			}
		})
	}
}
