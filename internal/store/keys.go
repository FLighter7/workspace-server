package store

import (
	"errors"
	"fmt"
	"strings"
)

// A document is the file <folder>/<id>.<type> in its app's folder; Key holds
// the three parts. The rules below keep every valid key a plain, portable
// file name on Windows, macOS and Linux alike, and keep it out of the
// server's own files:
//
//   - only [a-z0-9_-] in folder segments and ids and [a-z0-9] in types: no
//     uppercase, so two keys never name one file on a case-insensitive file
//     system; no dot, so the server's own .wss folder and the temporary
//     files it writes beside a document can never be a key, and "." and ".."
//     cannot occur; no space or dot at the end, which Windows would strip;
//     no "~", so no name looks like a Windows 8.3 short name;
//   - no Windows device name (CON, PRN, AUX, NUL, COM1-COM9, LPT1-LPT9) as a
//     folder segment or an id, where Windows would open the device instead of
//     the file (an id is the part before the dot, and "con.json" is the
//     device on Windows 10). The superscript variants (COM¹ and so on) cannot
//     be spelled in the charset. A type may be one: "x.con" is an ordinary
//     file;
//   - at most MaxFolderDepth folder levels and MaxKeyLen characters for the
//     whole key, which also keeps each name within the 255 a file system
//     allows.
const (
	// MaxFolderDepth is the most folder levels a key may have.
	MaxFolderDepth = 10
	// MaxIDLen is the longest id.
	MaxIDLen = 64
	// MaxTypeLen is the longest type.
	MaxTypeLen = 8
	// MaxKeyLen is the longest key, "folder/id.type" or "id.type" at the
	// app's root.
	MaxKeyLen = 255
)

// ErrBadPath is returned for a key that breaks the rules above. The protocol
// reports it as the error code badPath.
var ErrBadPath = errors.New("store: invalid document path")

// Key names a document: Path is its folder ("" for the app's root, segments
// joined by "/"), ID its name and Type its extension, without the dot. The
// type is part of the identity: a/r.jpg and a/r.png are two documents.
type Key struct {
	Path string
	ID   string
	Type string
}

// String returns the key's canonical form, "path/id.type", or "id.type" for a
// document at the app's root. It is the document's path relative to the app
// folder, with "/" as the separator.
func (k Key) String() string {
	if k.Path == "" {
		return k.ID + "." + k.Type
	}
	return k.Path + "/" + k.ID + "." + k.Type
}

// Validate returns nil for a valid key and an error wrapping ErrBadPath that
// names the part at fault otherwise.
func (k Key) Validate() error {
	switch {
	case !ValidFolder(k.Path):
		return fmt.Errorf("%w: folder %q", ErrBadPath, k.Path)
	case !ValidID(k.ID):
		return fmt.Errorf("%w: id %q", ErrBadPath, k.ID)
	case !ValidType(k.Type):
		return fmt.Errorf("%w: type %q", ErrBadPath, k.Type)
	case len(k.String()) > MaxKeyLen:
		return fmt.Errorf("%w: %q is longer than %d characters", ErrBadPath, k.String(), MaxKeyLen)
	}
	return nil
}

// ParseKey reads a file's path relative to the app folder, with "/" as the
// separator, back into a key. ok is false for any path that is not a valid
// key's String(): a file with such a name is not a document, and the server
// neither shows it nor touches it.
func ParseKey(rel string) (k Key, ok bool) {
	name := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		k.Path, name = rel[:i], rel[i+1:]
		if k.Path == "" {
			return Key{}, false // "/id.type": the root has no "/"
		}
	}
	dot := strings.IndexByte(name, '.')
	if dot < 0 {
		return Key{}, false
	}
	k.ID, k.Type = name[:dot], name[dot+1:]
	if k.Validate() != nil {
		return Key{}, false
	}
	return k, true
}

// ValidFolder reports whether folder is a valid folder of a key: "" (the
// app's root) or 1 to MaxFolderDepth segments of [a-z0-9_-] joined by "/",
// none of them a Windows device name. The length of the whole key is checked
// by Key.Validate.
func ValidFolder(folder string) bool {
	if folder == "" {
		return true
	}
	segments := strings.Split(folder, "/")
	if len(segments) > MaxFolderDepth {
		return false
	}
	for _, s := range segments {
		if !validName(s) {
			return false
		}
	}
	return true
}

// ValidID reports whether id is a valid document id: 1 to MaxIDLen
// characters of [a-z0-9_-], not a Windows device name.
func ValidID(id string) bool {
	return len(id) <= MaxIDLen && validName(id)
}

// ValidType reports whether typ is a valid document type: 1 to MaxTypeLen
// characters of [a-z0-9].
func ValidType(typ string) bool {
	if typ == "" || len(typ) > MaxTypeLen {
		return false
	}
	for i := 0; i < len(typ); i++ {
		if c := typ[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// validName reports whether s is a non-empty run of [a-z0-9_-] that is not a
// Windows device name.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return !windowsDevice(s)
}

// windowsDevice reports whether name is one of Windows' reserved device
// names, in any case: the charset allows only lowercase, but the comparison
// does not rely on that.
func windowsDevice(name string) bool {
	switch strings.ToUpper(name) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(name) == 4 && '1' <= name[3] && name[3] <= '9' {
		switch strings.ToUpper(name[:3]) {
		case "COM", "LPT":
			return true
		}
	}
	return false
}
