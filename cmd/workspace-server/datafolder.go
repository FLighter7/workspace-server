package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/FLighter7/workspace-server/internal/store"
)

// verdict is what changing the data folder to a path would do.
type verdict string

const (
	verdictSame        verdict = "same"        // it is the current folder
	verdictCreate      verdict = "create"      // missing; created, the current data moved in
	verdictMove        verdict = "move"        // no data there; the current data moves in
	verdictSwitch      verdict = "switch"      // no data on either side
	verdictAdopt       verdict = "adopt"       // only it has data, which becomes current
	verdictConflict    verdict = "conflict"    // both have data; refused
	verdictUnavailable verdict = "unavailable" // cannot be used; refused
)

// appData is an app's data in a data folder.
type appData struct {
	ID       string
	Size     int64 // the current document; 0 when there is none
	Versions int
}

// dataCheck is the result of checkDataDir.
type dataCheck struct {
	Path    string // the cleaned path
	Verdict verdict
	Message string // why, for a developer, in English
	Current []appData
	Target  []appData
}

// partialSuffix names the copy moveData builds before it renames it.
const partialSuffix = ".partial"

// checkDataDir tells what moving the data folder from current to target
// would do. It changes nothing but a probe file that tests whether target is
// writable. The error is for a failure to read the current folder; every
// problem with target is a verdict.
func checkDataDir(current, target string) (dataCheck, error) {
	c := dataCheck{Path: target}
	curData, err := hasData(current)
	if err != nil {
		return c, err
	}
	if c.Current, err = listAppData(current); err != nil {
		return c, err
	}
	refuse := func(format string, args ...any) (dataCheck, error) {
		c.Verdict, c.Message = verdictUnavailable, fmt.Sprintf(format, args...)
		return c, nil
	}
	if !filepath.IsAbs(target) {
		return refuse("%q is not an absolute path", target)
	}
	target = filepath.Clean(target)
	c.Path = target
	if err := checkDevDirsHere(target); err != nil {
		return refuse("%v", err)
	}
	if target == filepath.Clean(current) {
		c.Verdict = verdictSame
		return c, nil
	}
	if within(filepath.Join(current, storeDir), target) {
		return refuse("%s is inside the current data", target)
	}
	fi, err := os.Stat(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(target)
		if pfi, perr := os.Stat(parent); perr != nil || !pfi.IsDir() {
			return refuse("the folder %s does not exist", parent)
		}
		if err := probeWritable(parent); err != nil {
			return refuse("cannot create a folder in %s: %v", parent, err)
		}
		c.Verdict = verdictCreate
		return c, nil
	case err != nil:
		return refuse("%v", err)
	case !fi.IsDir():
		return refuse("%s is not a folder", target)
	}
	if os.SameFile(fi, statOrNil(current)) {
		c.Verdict = verdictSame
		return c, nil
	}
	if err := probeWritable(target); err != nil {
		return refuse("cannot write to %s: %v", target, err)
	}
	tgtData, err := hasData(target)
	if err != nil {
		return refuse("cannot read %s: %v", target, err)
	}
	if c.Target, err = listAppData(target); err != nil {
		return refuse("cannot read %s: %v", target, err)
	}
	switch {
	case curData && tgtData:
		c.Verdict, c.Message = verdictConflict, target+" already holds data of a workspace-server"
	case tgtData:
		c.Verdict = verdictAdopt
	case curData:
		c.Verdict = verdictMove
	default:
		c.Verdict = verdictSwitch
	}
	return c, nil
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func statOrNil(path string) os.FileInfo {
	fi, _ := os.Stat(path)
	return fi
}

// probeWritable creates and removes a file in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".workspace-server-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// The data folder's apps folder may hold more than the server's: apps is a
// common folder name, and the user may have chosen a folder that already has
// one. The server's own app folders are those with a valid app id that hold
// a document (data.json) or a history folder; everything else under apps is
// not data, and is never moved or deleted.

// appDirs returns the names of the server's app folders in dataDir, sorted.
func appDirs(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, storeDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || !apps.ValidID(e.Name()) {
			continue
		}
		dir := filepath.Join(dataDir, storeDir, e.Name())
		if isRegular(filepath.Join(dir, "data.json")) || isDir(filepath.Join(dir, "history")) {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func isRegular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// hasFiles reports whether a regular file lies anywhere below dir.
func hasFiles(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return found, err
}

// hasData reports whether dataDir holds data: a document or a history
// version in one of the server's app folders. Empty app folders, and
// anything that is not the server's, are not data.
func hasData(dataDir string) (bool, error) {
	ids, err := appDirs(dataDir)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		has, err := hasFiles(filepath.Join(dataDir, storeDir, id))
		if err != nil || has {
			return has, err
		}
	}
	return false, nil
}

// removeApps deletes the server's app folders in dataDir, and the empty
// folders with a valid app id, then the apps folder if nothing else is left
// in it. Anything else under apps stays.
func removeApps(dataDir string) error {
	root := filepath.Join(dataDir, storeDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ours, err := appDirs(dataDir)
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() || !apps.ValidID(e.Name()) {
			continue
		}
		if !slices.Contains(ours, e.Name()) {
			if has, err := hasFiles(dir); err != nil || has {
				continue // not the server's
			}
		}
		if err := os.RemoveAll(dir); err != nil && first == nil {
			first = err
		}
	}
	if left, err := os.ReadDir(root); err == nil && len(left) == 0 {
		if err := os.Remove(root); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// listAppData describes the apps with a valid id in dataDir that hold data.
func listAppData(dataDir string) ([]appData, error) {
	st := store.New(filepath.Join(dataDir, storeDir), time.Now)
	var out []appData
	for _, id := range adoptableApps(dataDir) {
		s, err := st.Stat(id)
		if err != nil {
			return nil, err
		}
		if s.Exists || s.Versions > 0 {
			out = append(out, appData{ID: id, Size: s.Size, Versions: s.Versions})
		}
	}
	return out, nil
}

// adoptableApps returns the ids of the server's app folders in dataDir,
// sorted. A missing folder has none.
func adoptableApps(dataDir string) []string {
	ids, _ := appDirs(dataDir)
	return ids
}

// moveData copies the server's app folders of from into to: first to
// apps.partial, fsynced, then into place, so that to never holds a half copy
// under the real name. An apps folder already in to must hold no data; its
// empty app folders are replaced, and anything that is not the server's
// stays. The source is left in place: the caller deletes it once the
// configuration points at to. On a failure nothing is added to to.
func moveData(from, to string) error {
	dst := filepath.Join(to, storeDir)
	partial := dst + partialSuffix
	if data, err := hasData(to); err != nil {
		return err
	} else if data {
		return fmt.Errorf("%s already holds data", to)
	}
	ids, err := appDirs(from)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("copy the data to %s: %w", to, err)
	}
	for _, id := range ids {
		if err := copyTree(filepath.Join(from, storeDir, id), filepath.Join(partial, id)); err != nil {
			return fail(err)
		}
	}
	syncTree(partial) // the copied files' folder entries, before they count
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(partial, dst); err != nil {
			return fail(err)
		}
		_ = fsutil.SyncDir(to)
		return nil
	}
	// An apps folder is there already: put each app into it. An app folder
	// of the same name is an empty leftover (there is no data in to).
	for _, id := range ids {
		target := filepath.Join(dst, id)
		if has, err := hasFiles(target); err != nil || has {
			if err == nil {
				err = fmt.Errorf("%s holds files that are not the server's", target)
			}
			return fail(err)
		}
		if err := os.RemoveAll(target); err != nil {
			return fail(err)
		}
	}
	for _, id := range ids {
		if err := os.Rename(filepath.Join(partial, id), filepath.Join(dst, id)); err != nil {
			return fail(err)
		}
	}
	_ = fsutil.SyncDir(dst)
	return os.RemoveAll(partial)
}

// syncTree syncs every folder below and at root, deepest first, best effort,
// so that the files copied into them survive a power loss.
func syncTree(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = fsutil.SyncDir(dirs[i])
	}
}

// copyTree copies the folders and regular files below src to dst, fsyncing
// each file. Anything else, such as a symbolic link, is skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o700)
		case d.Type().IsRegular():
			return copyFile(path, out)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// resetServerDir deletes what makes the server known to its devices: its
// identity, the app list, the paired devices, its link to an upstream server,
// and the server's app folders in dataDir.
// config.json, the log and the lock stay. It attempts every removal and
// returns the first error.
func resetServerDir(serverDir, dataDir string) error {
	var first error
	for _, path := range []string{
		filepath.Join(serverDir, identityFile),
		filepath.Join(serverDir, appsFile),
		filepath.Join(serverDir, devicesFile),
		filepath.Join(serverDir, upstreamFile),
		filepath.Join(serverDir, familyFile),
	} {
		if err := os.RemoveAll(path); err != nil && first == nil {
			first = err
		}
	}
	if err := removeApps(dataDir); err != nil && first == nil {
		first = err
	}
	return first
}

// serverDirMarkers are the files of which a server folder holds at least
// one; removeServerDir refuses a folder with none of them.
var serverDirMarkers = []string{identityFile, configFile, appsFile, lockFile}

// removeServerDir deletes the server folder dir. It refuses a folder that
// holds none of serverDirMarkers, so a mistyped -dir cannot wipe an unrelated
// folder, and a folder another process has locked (errLocked). The lock file
// goes last, after the lock is released: removing it while a server could
// still hold it would let two servers run on the folder. A missing folder is
// nothing to do.
func removeServerDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	ours := false
	for _, name := range serverDirMarkers {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			ours = true
			break
		}
	}
	if !ours {
		return fmt.Errorf("%s is not a workspace-server folder: it holds none of %s", dir, strings.Join(serverDirMarkers, ", "))
	}
	unlock, lockErr := lockServerDir(dir)
	if errors.Is(lockErr, errLocked) {
		return fmt.Errorf("a server still uses %s: %w", dir, lockErr)
	}
	// Any other lock error means the file system has no locks; the server
	// runs without one there too.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.Name() == lockFile {
				continue
			}
			if rerr := os.RemoveAll(filepath.Join(dir, e.Name())); rerr != nil && err == nil {
				err = rerr
			}
		}
	}
	if lockErr == nil {
		unlock()
	}
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, lockFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(dir)
}

// removeData deletes the apps' data in a data folder other than the server
// folder: the server's app folders (see removeApps), and then the folder
// itself only if nothing else is left in it. The user may have chosen a folder that holds other files. A
// missing folder is nothing to do.
func removeData(dataDir string) error {
	if err := removeApps(dataDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(dataDir)
	}
	return nil
}
