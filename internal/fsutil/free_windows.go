package fsutil

import "golang.org/x/sys/windows"

// FreeSpace returns the bytes the user of the process can still write on the
// volume that holds dir: its quota is counted, as the free space of the volume
// alone would overstate it.
func FreeSpace(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return int64(free), nil
}
