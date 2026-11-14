package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/FLighter7/workspace-server/internal/apps"
)

// The Apps tab's facts: each app's folder under the apps root, no
// divergence and no failing sync on a main.
func TestAppsListTheFolderAndNoDivergence(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	list := s.Apps()
	if len(list) != 1 || list[0].ID != "todo" {
		t.Fatalf("Apps() = %+v, want todo alone", list)
	}
	todo := list[0]
	if want := filepath.Join(dir, "WSS-Data", "apps", "todo"); todo.Folder != want {
		t.Errorf("folder = %q, want %q", todo.Folder, want)
	}
	if todo.Diverged == nil || len(todo.Diverged) != 0 || todo.Failing != "" {
		t.Errorf("diverged %#v, failing %q; want [] and none", todo.Diverged, todo.Failing)
	}
	if todo.Quota != apps.DefaultQuota {
		t.Errorf("quota = %d, want the default", todo.Quota)
	}
}

// A main sets an app's quota in apps.json; the registry's refusals pass
// through, and an unchanged quota writes nothing.
func TestSetQuotaOnAMain(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	if err := s.SetQuota("todo", 3<<20); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.apps.Get("todo"); a.Quota != 3<<20 {
		t.Errorf("quota = %d, want 3 MiB", a.Quota)
	}
	reg, err := apps.Open(filepath.Join(dir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := reg.Get("todo"); a.Quota != 3<<20 {
		t.Errorf("apps.json holds quota %d, want 3 MiB", a.Quota)
	}
	if err := s.SetQuota("todo", 0); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.apps.Get("todo"); a.Quota != apps.DefaultQuota {
		t.Errorf("a zero quota left %d, want the default", a.Quota)
	}
	if err := s.SetQuota("todo", 1024); !errors.Is(err, apps.ErrInvalid) {
		t.Errorf("a quota under 1 MiB: %v, want ErrInvalid", err)
	}
	if err := s.SetQuota("notes", 2<<20); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("an unknown app: %v, want ErrNotFound", err)
	}
}

// OpenFolder opens the app's folder, made if it is missing, and nothing
// for an app the server does not serve.
func TestOpenFolderOpensTheAppsFolder(t *testing.T) {
	s, dir := newTestServer(t, io.Discard)
	var opened []string
	s.openFolder = func(path string) error {
		opened = append(opened, path)
		return nil
	}
	if err := s.OpenFolder("todo"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "WSS-Data", "apps", "todo")
	if !slices.Equal(opened, []string{want}) {
		t.Errorf("opened %v, want [%s]", opened, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("the app's folder: %v", err)
	}
	if err := s.OpenFolder("notes"); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("an unknown app: %v, want ErrNotFound", err)
	}
	if err := s.OpenFolder("../todo"); !errors.Is(err, apps.ErrNotFound) {
		t.Errorf("a path for an id: %v, want ErrNotFound", err)
	}
	if len(opened) != 1 {
		t.Errorf("opened %v after the refusals", opened)
	}
	s.openFolder = func(string) error { return errors.New("no file manager") }
	if err := s.OpenFolder("todo"); err == nil {
		t.Error("a failing opener reported success")
	}
}
