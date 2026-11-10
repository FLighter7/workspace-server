package datadir

import "testing"

func TestDefaultFor(t *testing.T) {
	env := map[string]string{"APPDATA": `C:\Users\a\AppData\Roaming`, "XDG_DATA_HOME": "/x"}
	getenv := func(k string) string { return env[k] }
	cases := []struct{ goos, want string }{
		{"darwin", "/home/a/Library/Application Support/workspace-server"},
		{"windows", `C:\Users\a\AppData\Roaming` + `\workspace-server`},
		{"linux", "/x/workspace-server"},
	}
	for _, c := range cases {
		if got := defaultFor(c.goos, "/home/a", getenv); got != c.want {
			t.Errorf("%s: got %q, want %q", c.goos, got, c.want)
		}
	}
	if got := defaultFor("linux", "/home/a", func(string) string { return "" }); got != "/home/a/.local/share/workspace-server" {
		t.Errorf("linux fallback: got %q", got)
	}
}

func TestDefaultForWindowsWithoutAppData(t *testing.T) {
	got := defaultFor("windows", `C:\Users\a`, func(string) string { return "" })
	if want := `C:\Users\a\AppData\Roaming\workspace-server`; got != want {
		t.Errorf("defaultFor(windows, no APPDATA) = %q, want %q", got, want)
	}
}
