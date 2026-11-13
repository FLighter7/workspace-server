package store

import (
	"errors"
	"strings"
	"testing"
)

// levels returns n folder segments "a/a/...".
func levels(n int) string { return strings.TrimSuffix(strings.Repeat("a/", n), "/") }

func TestValidFolder(t *testing.T) {
	for _, tc := range []struct {
		folder string
		want   bool
	}{
		{"", true},
		{"photos", true},
		{"a/b-c/d_e/0-9", true},
		{levels(10), true},
		{levels(11), false},
		{"Photos", false},
		{"a/B", false},
		{"a.b", false},
		{".wss", false},
		{"a/.wss", false},
		{".", false},
		{"..", false},
		{"a/../b", false},
		{"a b", false},
		{"a~1", false},
		{`a\b`, false},
		{"a:b", false},
		{"é", false},
		{"/a", false},
		{"a/", false},
		{"/", false},
		{"a//b", false},
		{"con", false},
		{"CON", false},
		{"a/nul", false},
		{"prn/a", false},
		{"aux", false},
		{"com1", false},
		{"lpt9", false},
		{"com0", true},
		{"com10", true},
		{"cons", true},
		{"lpt", true},
	} {
		if got := ValidFolder(tc.folder); got != tc.want {
			t.Errorf("ValidFolder(%q) = %v, want %v", tc.folder, got, tc.want)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"a", true},
		{"data", true},
		{"2026-11-13_x", true},
		{"-", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"Data", false},
		{"a.b", false},
		{"a/b", false},
		{"a b", false},
		{"a$", false},
		{"con", false},
		{"CON", false},
		{"Con", false},
		{"nul", false},
		{"com1", false},
		{"lpt9", false},
		{"conin", true},
	} {
		if got := ValidID(tc.id); got != tc.want {
			t.Errorf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestValidType(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"json", true},
		{"jpg", true},
		{"mp4", true},
		{"7z", true},
		{"con", true},
		{strings.Repeat("a", 8), true},
		{strings.Repeat("a", 9), false},
		{"", false},
		{"JSON", false},
		{"tar.gz", false},
		{".json", false},
		{"a-b", false},
		{"a_b", false},
		{"a/b", false},
	} {
		if got := ValidType(tc.typ); got != tc.want {
			t.Errorf("ValidType(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestKeyValidate(t *testing.T) {
	id64, type8 := strings.Repeat("i", 64), strings.Repeat("t", 8)
	// A folder of n characters plus "/", the id, "." and the type: n + 74.
	long := func(n int) Key { return Key{Path: strings.Repeat("f", n), ID: id64, Type: type8} }
	if got := len(long(181).String()); got != 255 {
		t.Fatalf("long(181) is %d characters", got)
	}
	for _, tc := range []struct {
		key  Key
		want bool
	}{
		{Key{"", "data", "json"}, true},
		{Key{"a/b", "r", "jpg"}, true},
		{Key{levels(10), "x", "bin"}, true},
		{long(181), true},
		{long(182), false},
		{Key{"", strings.Repeat("a", 246), "json"}, false}, // the id's own limit first
		{Key{levels(11), "x", "bin"}, false},
		{Key{"", "data", ""}, false},
		{Key{"", "", "json"}, false},
		{Key{"", "data", "jsonjsonj"}, false},
		{Key{"", "Data", "json"}, false},
		{Key{"", "con", "json"}, false},
		{Key{"nul", "data", "json"}, false},
		{Key{"a/", "data", "json"}, false},
		{Key{"/a", "data", "json"}, false},
		{Key{".wss", "index", "json"}, false},
	} {
		err := tc.key.Validate()
		if (err == nil) != tc.want {
			t.Errorf("%+v: Validate() = %v, want valid %v", tc.key, err, tc.want)
		}
		if err != nil && !errors.Is(err, ErrBadPath) {
			t.Errorf("%+v: %v does not wrap ErrBadPath", tc.key, err)
		}
	}
}

func TestKeyString(t *testing.T) {
	if got := (Key{"", "data", "json"}).String(); got != "data.json" {
		t.Errorf("root key = %q", got)
	}
	if got := (Key{"a/b", "r", "jpg"}).String(); got != "a/b/r.jpg" {
		t.Errorf("nested key = %q", got)
	}
}

func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		rel  string
		want Key
		ok   bool
	}{
		{"data.json", Key{"", "data", "json"}, true},
		{"a/b/r.jpg", Key{"a/b", "r", "jpg"}, true},
		{levels(10) + "/x.bin", Key{levels(10), "x", "bin"}, true},
		{levels(11) + "/x.bin", Key{}, false},
		{"data", Key{}, false},
		{"data.", Key{}, false},
		{".json", Key{}, false},
		{"a.tar.gz", Key{}, false},
		{"Data.json", Key{}, false},
		{"data.JSON", Key{}, false},
		{"/data.json", Key{}, false},
		{"a//data.json", Key{}, false},
		{"a/", Key{}, false},
		{"", Key{}, false},
		{".wss/index.json", Key{}, false},
		{"data.json.tmp-123", Key{}, false},
		{"con.json", Key{}, false},
		{"lpt1/a.json", Key{}, false},
		{`a\b.json`, Key{}, false},
		{"photos/" + strings.Repeat("a", 65) + ".jpg", Key{}, false},
	} {
		got, ok := ParseKey(tc.rel)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseKey(%q) = %+v, %v; want %+v, %v", tc.rel, got, ok, tc.want, tc.ok)
		}
	}
}

// Every valid key reads back from its own String().
func TestParseKeyRoundTrip(t *testing.T) {
	for _, k := range []Key{
		{"", "data", "json"},
		{"a", "b", "c"},
		{"x-1/y_2", "-", "7z"},
		{levels(10), strings.Repeat("i", 64), strings.Repeat("t", 8)},
		{strings.Repeat("f", 181), strings.Repeat("i", 64), strings.Repeat("t", 8)},
	} {
		if err := k.Validate(); err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
		if got, ok := ParseKey(k.String()); !ok || got != k {
			t.Errorf("ParseKey(%q) = %+v, %v; want %+v", k.String(), got, ok, k)
		}
	}
}
