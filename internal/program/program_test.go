package program

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEntry(t *testing.T) {
	exe := `C:\Users\a b\Downloads\workspace-server.exe`
	one := uint32(1)
	want := []Value{
		{Name: "DisplayName", String: "workspace-server"},
		{Name: "DisplayVersion", String: "v1.2.3"},
		{Name: "Publisher", String: "FLighter7"},
		{Name: "InstallLocation", String: `C:\Users\a b\Downloads`},
		{Name: "DisplayIcon", String: exe},
		{Name: "UninstallString", String: `"C:\Users\a b\Downloads\workspace-server.exe" delete --pause`},
		{Name: "NoModify", DWord: &one},
		{Name: "NoRepair", DWord: &one},
	}
	got := Entry(exe, "v1.2.3")
	if len(got) != len(want) {
		t.Fatalf("Entry has %d values, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.Name || g.String != w.String || (g.DWord == nil) != (w.DWord == nil) ||
			(g.DWord != nil && *g.DWord != *w.DWord) {
			t.Errorf("value %d = %+v (dword %v), want %+v (dword %v)", i, g, deref(g.DWord), w, deref(w.DWord))
		}
		if w.DWord != nil && g.String != "" {
			t.Errorf("%s is a DWORD and must carry no string", g.Name)
		}
	}
}

func deref(p *uint32) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestHelperScriptQuotesThePath(t *testing.T) {
	for _, c := range []struct{ path, quoted string }{
		{`C:\Program Files\a b\ws.exe`, `'C:\Program Files\a b\ws.exe'`},
		{`C:\it's\ws.exe`, `'C:\it''s\ws.exe'`},
		{"C:\\$x`y\\ws.exe", "'C:\\$x`y\\ws.exe'"},
		// PowerShell takes the typographic single quotes for ' too.
		{"C:\\O\u2019Brien\\ws.exe", "'C:\\O\u2019\u2019Brien\\ws.exe'"},
		{"C:\\a\u2018b\u201ac\u201bd\\ws.exe", "'C:\\a\u2018\u2018b\u201a\u201ac\u201b\u201bd\\ws.exe'"},
	} {
		s := helperScript(c.path)
		if n := strings.Count(s, c.quoted); n != 1 {
			t.Errorf("%q: the quoted path %s appears %d times in\n%s", c.path, c.quoted, n, s)
		}
		// The path is named only through the variable, in single quotes, so
		// nothing in it is interpreted.
		if !strings.Contains(s, "Remove-Item -LiteralPath $p") || !strings.Contains(s, "$p = "+c.quoted) {
			t.Errorf("%q: the script must assign the quoted path to $p and remove -LiteralPath $p:\n%s", c.path, s)
		}
		if strings.Contains(s, `"`) {
			t.Errorf("%q: the script holds a double quote, which PowerShell would expand:\n%s", c.path, s)
		}
	}
}

func TestRemoveDeletesARegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows removes through a helper after the process exits")
	}
	path := filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there: %v", err)
	}
}

func TestRemoveRefusesADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Remove(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the folder is gone: %v", err)
	}
}

func TestRemoveRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ws")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := Remove(link); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Remove(link) = %v, want ErrNotRegular", err)
	}
	for _, p := range []string{target, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is gone: %v", p, err)
		}
	}
}

func TestRemoveReportsAMissingFile(t *testing.T) {
	err := Remove(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove(missing) = %v, want an error wrapping os.ErrNotExist", err)
	}
}

func TestRegisterAndUnregisterDoNothingOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("they write the registry on Windows")
	}
	if err := Register("/opt/ws", "dev"); err != nil {
		t.Errorf("Register = %v", err)
	}
	if err := Unregister(); err != nil {
		t.Errorf("Unregister = %v", err)
	}
}
