package devices

import (
	"errors"
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

// linkedLaptop is a registry's server-kind device for laptop().
func linkedLaptop(t *testing.T, r *Registry) Device {
	t.Helper()
	d, _, err := r.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetServer(d.ID, laptop()); err != nil {
		t.Fatal(err)
	}
	return d
}

// shownName is what r calls its device id.
func shownName(t *testing.T, r *Registry, id string) string {
	t.Helper()
	d, ok := r.Get(id)
	if !ok {
		t.Fatalf("no device %s", id)
	}
	for _, l := range r.List() {
		if l.ID == id && l.ShownName() != d.ShownName() {
			t.Errorf("List names %q, Get %q", l.ShownName(), d.ShownName())
		}
	}
	return d.ShownName()
}

// The user's name for a server is shown for the one it announced, also once
// it announces another, and to the devices and the family; it is kept on
// disk, and goes with the server, or when the user types the announced name.
func TestRenameServer(t *testing.T) {
	r, path, _ := openFresh(t)
	srv, _, err := r.PairServer("Laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RenameServer(srv.ID, "Work"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("a server that has not announced itself: %v, want ErrNoDevice", err)
	}
	if _, err := r.SetServer(srv.ID, laptop()); err != nil {
		t.Fatal(err)
	}
	if got := shownName(t, r, srv.ID); got != "Laptop" {
		t.Errorf("announced, it is %q, want Laptop", got)
	}
	if err := r.RenameServer(srv.ID, "  Work laptop "); err != nil {
		t.Fatal(err)
	}
	announced := laptop()
	announced.Name = "Laptop 2"
	if _, err := r.SetServer(srv.ID, announced); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []*Registry{r, again} {
		if got := shownName(t, reg, srv.ID); got != "Work laptop" {
			t.Errorf("renamed, it is %q, want Work laptop", got)
		}
		if got := reg.Subordinates(); len(got) != 1 || got[0].Name != "Work laptop" {
			t.Errorf("Subordinates = %+v, want it under the user's name", got)
		}
		e := reg.Entries()
		if len(e) != 1 || e[0].ServerName != "Work laptop" || e[0].Server.Name != "Laptop 2" {
			t.Errorf("Entries = %+v, want the user's name beside the announced one", e)
		}
	}

	if err := r.RenameServer(srv.ID, " "); !errors.Is(err, ErrInvalidName) {
		t.Errorf("an empty name: %v, want ErrInvalidName", err)
	}
	if err := r.RenameServer("NoSuchDevice", "x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("an unknown device: %v, want ErrNoDevice", err)
	}
	phone, _ := mustPair(t, r, "todo", "Phone")
	if err := r.RenameServer(phone.ID, "x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("a device of an app: %v, want ErrNoDevice", err)
	}

	if err := r.RenameServer(srv.ID, "Laptop 2"); err != nil {
		t.Fatal(err)
	}
	if e := r.Entries(); e[0].ServerName != "" || strings.Contains(string(readFile(t, path)), "serverNames") {
		t.Errorf("under its own name again it has the user's: %+v", e[0])
	}

	if err := r.RenameServer(srv.ID, "Work"); err != nil {
		t.Fatal(err)
	}
	r.Remove(srv.ID)
	back := linkedLaptop(t, r)
	if got := shownName(t, r, back.ID); got != "Laptop" {
		t.Errorf("removed and linked again, it is %q, want its own name", got)
	}
}

// The role of main goes to the server the user named and comes back, and the
// name is still there; the new main's word on the others' names holds.
func TestServerNamesThroughASwitch(t *testing.T) {
	desk, path, _ := openFresh(t)
	lap := linkedLaptop(t, desk)
	if err := desk.RenameServer(lap.ID, "Work laptop"); err != nil {
		t.Fatal(err)
	}
	nas := Entry{ID: "Srv0000000000nas", Kind: KindServer, Name: "NAS", TokenHash: strings.Repeat("a", 64), ServerName: "Cellar"}
	nasServer := laptop()
	nasServer.ServerID, nasServer.Name = "Nas0Nas0Nas0", "NAS"
	nas.Server = &nasServer
	if _, err := desk.Merge([]Entry{nas}, nil, Self{}); err != nil {
		t.Fatal(err)
	}
	if got := shownName(t, desk, nas.ID); got != "Cellar" {
		t.Errorf("a server merged in is %q, want the name it came with", got)
	}

	// The laptop takes the role over: the desk's registry becomes its own.
	taken, _, _ := openFresh(t)
	taken.SetReplica(desk.Entries())
	if _, err := taken.Merge(taken.Replica(), nil, Self{ServerID: laptop().ServerID}); err != nil {
		t.Fatal(err)
	}
	if got := shownName(t, taken, nas.ID); got != "Cellar" {
		t.Errorf("on the new main it is %q, want Cellar", got)
	}
	if err := taken.RenameServer(nas.ID, "Attic"); err != nil {
		t.Fatal(err)
	}

	// The desk follows it, and then is the main again.
	if err := desk.Clear(); err != nil {
		t.Fatal(err)
	}
	desk.SetReplica(taken.Entries())
	if _, err := desk.Merge(desk.Replica(), nil, Self{}); err != nil {
		t.Fatal(err)
	}
	back, _, err := desk.MintServer(laptop())
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Registry{desk, again} {
		if got := shownName(t, r, back.ID); got != "Work laptop" {
			t.Errorf("the laptop is %q, want Work laptop", got)
		}
		if got := shownName(t, r, nas.ID); got != "Attic" {
			t.Errorf("the NAS is %q, want the name given on the other main", got)
		}
	}

	// Removed by the family, the server takes its name with it.
	if _, err := desk.Merge(nil, []Tombstone{{ID: nas.ID, At: newClock().now()}}, Self{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readFile(t, path)), "Attic") {
		t.Error("a removed server's name is still on disk")
	}
}

func TestServerNameValidate(t *testing.T) {
	hash := strings.Repeat("d", 64)
	if err := (Entry{ID: "a", Kind: KindServer, TokenHash: hash, ServerName: "Work"}).Validate(); err != nil {
		t.Errorf("a server's name: %v", err)
	}
	if (Entry{ID: "a", App: "todo", TokenHash: hash, ServerName: "Work"}).Validate() == nil {
		t.Error("a device of an app with a server's name was valid")
	}
	if (Entry{ID: "a", Kind: KindServer, TokenHash: hash, ServerName: strings.Repeat("я", 65)}).Validate() == nil {
		t.Error("a server's name over 64 characters was valid")
	}
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"devices":[],"serverNames":{"not an id":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, newClock().now); err == nil {
		t.Error("a name of a server whose id is not valid was read")
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
		{ID: notes.ID, App: "notes", Name: "Phone", TokenHash: notes.TokenHash, PairedAt: notes.PairedAt, Machine: notes.Machine, MachineName: "Phone"},
		{ID: todo.ID, App: "todo", Name: "Phone", TokenHash: todo.TokenHash, PairedAt: todo.PairedAt, Machine: todo.Machine, MachineName: "Phone"},
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
