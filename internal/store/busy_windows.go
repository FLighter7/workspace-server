package store

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isBusy reports whether err is what Windows returns when another handle
// keeps a rename or a removal from going through: access denied (5), a
// sharing violation (32) or a lock violation (33). A handle that another
// program opened without FILE_SHARE_DELETE (an editor, a virus scanner, a
// backup tool) is usually closed again soon.
func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
