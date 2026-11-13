package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OpenShared opens the file name for reading so that, while the handle stays
// open, the name can be renamed over (by Rename) or removed: the handle keeps
// reading the old file to the end, and the name gets the new one at once.
//
// os.Open does not do this: it shares read and write but not delete, and one
// such handle on a document makes every rename over it or removal of it fail
// with ERROR_SHARING_VIOLATION until it closes, so a commit would wait on a
// slow reader. That is why every read of a document goes through OpenShared
// (or ReadFileShared), never os.Open or os.ReadFile. A handle opened by
// another process without FILE_SHARE_DELETE (an editor, a backup tool) still
// blocks the commit; nothing on this side can change that.
//
// The handle is synchronous, so the returned *os.File reads, seeks and stats
// like any other.
func OpenShared(name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(longPath(name))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}

// Rename renames oldpath to newpath, replacing newpath if it exists, with
// POSIX semantics: it succeeds while newpath is open through OpenShared, the
// open handle keeps the old file, and a fresh open of newpath sees the new
// one at once.
//
// os.Rename (MoveFileEx) uses the legacy replace, which fails with
// ERROR_ACCESS_DENIED over a target that is open, even through OpenShared.
// POSIX renames need Windows 10 1809 or later and a file system that has them
// (NTFS has; FAT, exFAT and many network shares do not): there Rename falls
// back to os.Rename, which replaces newpath only while nothing has it open.
// Over a target opened without FILE_SHARE_DELETE (a plain os.Open) Rename
// fails with ERROR_SHARING_VIOLATION and leaves both files as they were.
func Rename(oldpath, newpath string) error {
	err := renamePOSIX(oldpath, newpath)
	if posixUnsupported(err) {
		return os.Rename(oldpath, newpath)
	}
	return err
}

// posixUnsupported reports whether err is what a rename through
// FileRenameInfoEx returns where Windows (before 1809: an unknown information
// class) or the file system (no POSIX semantics) does not support it. These
// are the NTSTATUS codes on which Go's own os.Root rename and os.Remove fall
// back, as Win32 errors.
func posixUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

// fileRenameInfo is FILE_RENAME_INFO as SetFileInformationByHandle takes it
// for FileRenameInfoEx: the union's DWORD Flags, the root directory, the new
// name's length in bytes, then the name itself, which runs past the struct.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func renamePOSIX(oldpath, newpath string) error {
	linkErr := func(err error) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	from, err := windows.UTF16PtrFromString(longPath(oldpath))
	if err != nil {
		return linkErr(err)
	}
	// The new name is resolved without a root directory handle, so it has to
	// be absolute.
	abs, err := filepath.Abs(newpath)
	if err != nil {
		return linkErr(err)
	}
	to, err := windows.UTF16FromString(longPath(abs))
	if err != nil {
		return linkErr(err)
	}
	// FILE_FLAG_OPEN_REPARSE_POINT renames a link itself, as os.Rename does;
	// FILE_FLAG_BACKUP_SEMANTICS lets the handle be a directory's.
	h, err := windows.CreateFile(from, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return linkErr(err)
	}
	defer windows.CloseHandle(h)

	nameBytes := (len(to) - 1) * 2 // without the terminating NUL
	size := int(unsafe.Offsetof(fileRenameInfo{}.FileName)) + nameBytes + 2
	buf := make([]byte, size)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&info.FileName[0], len(to)), to)
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(size)); err != nil {
		return linkErr(err)
	}
	return nil
}

// longPath gives a path of 248 bytes or more the extended-length prefix, as
// the os package does for its own calls, so that a document deep in the data
// folder (a key alone may be 255 characters) opens and renames past MAX_PATH.
// The prefix turns off Win32 path normalisation, hence filepath.Abs first,
// which also cleans the path. A path that cannot be made absolute is returned
// as it is, and the call fails or succeeds on its own.
func longPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	switch {
	case strings.HasPrefix(abs, `\\.\`):
		return abs // a device path: the prefix would change its meaning
	case strings.HasPrefix(abs, `\\`):
		return `\\?\UNC\` + abs[2:]
	default:
		return `\\?\` + abs
	}
}
