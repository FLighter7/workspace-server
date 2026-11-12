package devices

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func laptop() Server {
	return Server{
		ServerID: "Lp7tOp0123ab", Name: "Laptop", Password: "Zx9yW8vU7tS6rQ5pO4nM3lK2",
		Fingerprint: "10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F:20:21:22:23:24:25:26:27:28:29:2A:2B:2C:2D:2E:2F",
		Port:        47801, Hosts: []string{"5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f.local", "192.168.1.30"},
	}
}

func TestServerValidate(t *testing.T) {
	if err := laptop().Validate(); err != nil {
		t.Fatalf("a valid server: %v", err)
	}
	many := make([]string, MaxServerHosts+1)
	for i := range many {
		many[i] = "10.0.0.1"
	}
	for name, change := range map[string]func(*Server){
		"short serverId":     func(s *Server) { s.ServerID = "abc" },
		"empty name":         func(s *Server) { s.Name = "  " },
		"long name":          func(s *Server) { s.Name = strings.Repeat("я", 65) },
		"short password":     func(s *Server) { s.Password = "abc" },
		"lowercase hex":      func(s *Server) { s.Fingerprint = strings.ToLower(s.Fingerprint) },
		"port 0":             func(s *Server) { s.Port = 0 },
		"no hosts":           func(s *Server) { s.Hosts = nil },
		"too many hosts":     func(s *Server) { s.Hosts = many },
		"a host with a line": func(s *Server) { s.Hosts = []string{"a.local\r\na=x"} },
	} {
		s := laptop()
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

// What a server-kind device announces is kept with it, written only when it
// changes, listed by Subordinates, and read back; a device of an app has
// none.
func TestSetServerAndSubordinates(t *testing.T) {
	r, path, _ := openFresh(t)
	srv, _, err := r.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	phone, _, err := r.Pair("todo", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Subordinates(); got == nil || len(got) != 0 {
		t.Errorf("before any announce: %v, want [] (not nil)", got)
	}
	if changed, err := r.SetServer(srv.ID, laptop()); err != nil || !changed {
		t.Fatalf("SetServer = %v, %v", changed, err)
	}
	before := readFile(t, path)
	if changed, err := r.SetServer(srv.ID, laptop()); err != nil || changed {
		t.Errorf("the same again = %v, %v; want no change", changed, err)
	}
	if !reflect.DeepEqual(readFile(t, path), before) {
		t.Error("the same announce rewrote the file")
	}
	if _, err := r.SetServer(phone.ID, laptop()); err == nil {
		t.Error("a device of an app took a server")
	}
	bad := laptop()
	bad.Port = 0
	if _, err := r.SetServer(srv.ID, bad); err == nil {
		t.Error("an invalid server was kept")
	}

	again, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Subordinates(); !reflect.DeepEqual(got, []Server{laptop()}) {
		t.Errorf("after a reopen: %+v", got)
	}
	r.Remove(srv.ID)
	if got := r.Subordinates(); len(got) != 0 {
		t.Errorf("after the removal: %+v", got)
	}
}

func TestOpenRejectsAServerEntry(t *testing.T) {
	for name, entry := range map[string]string{
		"on a device of an app": `{"id":"a","app":"todo","name":"x","tokenHash":"` + strings.Repeat("0", 64) + `","server":` + serverJSON(t, laptop()) + `}`,
		"not valid":             `{"id":"a","app":"","kind":"server","name":"x","tokenHash":"` + strings.Repeat("0", 64) + `","server":{"serverId":"x"}}`,
	} {
		path := filepath.Join(t.TempDir(), "devices.json")
		if err := os.WriteFile(path, []byte(`{"devices":[`+entry+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, newClock().now); err == nil || !strings.Contains(err.Error(), "devices[0]") {
			t.Errorf("%s: Open = %v, want an error naming the entry", name, err)
		}
	}
}

func serverJSON(t *testing.T, s Server) string {
	t.Helper()
	return `{"serverId":"` + s.ServerID + `","name":"` + s.Name + `","password":"` + s.Password +
		`","fingerprint":"` + s.Fingerprint + `","port":47801,"hosts":["a.local"]}`
}

// Entries is the own devices, server-kind ones first, then sorted by app and
// id, with their hashes and labels: never the replica.
func TestEntries(t *testing.T) {
	r, _, _ := openFresh(t)
	todo, _, _ := r.Pair("todo", "Phone")
	notes, _, _ := r.Pair("notes", "Phone")
	laptop, _, err := r.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	r.SetReplica([]Entry{{ID: "Dev0123456789ab", App: "todo", TokenHash: strings.Repeat("a", 64)}})
	want := []Entry{
		{ID: laptop.ID, App: "", Kind: KindServer, Name: "Laptop", TokenHash: laptop.TokenHash, PairedAt: laptop.PairedAt},
		{ID: notes.ID, App: "notes", Name: "Phone", TokenHash: notes.TokenHash, PairedAt: notes.PairedAt},
		{ID: todo.ID, App: "todo", Name: "Phone", TokenHash: todo.TokenHash, PairedAt: todo.PairedAt},
	}
	if got := r.Entries(); !reflect.DeepEqual(got, want) {
		t.Errorf("Entries = %+v, want %+v", got, want)
	}
}

// The replica is admitted by its hashes, never listed or written, and an
// own device wins an id the two share.
func TestReplica(t *testing.T) {
	r, path, _ := openFresh(t)
	own, ownToken, _ := r.Pair("todo", "Phone")
	r.SetReplica([]Entry{
		{ID: "Dev0123456789ab", App: "todo", TokenHash: hashToken("tok")},
		{ID: own.ID, App: "todo", TokenHash: hashToken("other")},
		{ID: "x", App: "Bad App", TokenHash: hashToken("tok")},
	})
	if d, ok := r.Authenticate("todo", "Dev0123456789ab", "tok"); !ok || !d.Replica {
		t.Errorf("Authenticate the main's device = %+v, %v", d, ok)
	}
	if _, ok := r.Authenticate("notes", "Dev0123456789ab", "tok"); ok {
		t.Error("the main's device was admitted to another app")
	}
	if _, ok := r.Authenticate("todo", own.ID, "other"); ok {
		t.Error("the replica's hash won over an own device's")
	}
	if _, ok := r.Authenticate("todo", own.ID, ownToken); !ok {
		t.Error("the own device lost its id to the replica")
	}
	if n := len(r.Replica()); n != 2 {
		t.Errorf("replica holds %d, want the 2 valid entries", n)
	}
	if list := r.List(); len(list) != 1 {
		t.Errorf("List = %+v, want the own device only", list)
	}
	if r.Remove("Dev0123456789ab") {
		t.Error("Remove removed a device of the replica")
	}
	if strings.Contains(string(readFile(t, path)), "Dev0123456789ab") {
		t.Error("the replica was written to devices.json")
	}
	r.SetReplica(nil)
	if _, ok := r.Get("Dev0123456789ab"); ok {
		t.Error("the replica kept a device after SetReplica(nil)")
	}
}
