package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

	// The WSS-Data folders of both, resolved once by the check: what a
	// folder is taken for depends on what it holds, which a move changes.
	curWSS, tgtWSS string
	curData        bool // the current folder holds data
}

// partialSuffix names the copy moveData builds before it renames it.
const partialSuffix = ".partial"

// readmeText is WSS-Data's README.txt: what the folder is, for a person who
// comes across it.
const readmeText = `WSS-Data: your workspace-server data

This folder holds the data of the apps that sync through workspace-server
on this computer. Each app's files are in apps/<app id>/.

Do not change, move, rename or delete this folder or anything in it.
The server and the apps on your devices rely on it exactly as it is:
a change made here can lose your data.

To move your data to another folder, change the data folder on the
server's page (Server → Data folder); the server moves it for you.
`

// wssDir returns the WSS-Data folder of the data folder dataDir, the one
// that holds the apps' data: dataDir/WSS-Data, or dataDir itself when the
// user chose a WSS-Data folder, so that there is no WSS-Data/WSS-Data. A
// folder is taken for one when it is named WSS-Data (in any case where the
// file system ignores case) and holds apps or README.txt, or nothing at all
// but the system's own files (missing or empty): a WSS-Data folder that a
// deletion emptied, or one just being made, stays the data's folder.
//
// The answer depends on what the folder holds, so a caller that writes into
// these folders resolves them once, before it writes.
func wssDir(dataDir string) string {
	if isWSSName(filepath.Base(dataDir)) && isWSSFolder(dataDir) {
		return dataDir
	}
	return filepath.Join(dataDir, wssDirName)
}

// isWSSName reports whether name is WSS-Data, ignoring case on Windows and
// macOS, whose file systems do.
func isWSSName(name string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(name, wssDirName)
	}
	return name == wssDirName
}

// systemFiles are the files Windows and macOS leave in a folder someone
// merely looked at; they do not make a folder hold something.
var systemFiles = []string{".DS_Store", "desktop.ini", "Thumbs.db"}

func isSystemFile(name string) bool {
	return slices.ContainsFunc(systemFiles, func(s string) bool { return strings.EqualFold(s, name) })
}

// isWSSFolder reports whether dir holds apps or README.txt, or nothing but
// systemFiles.
func isWSSFolder(dir string) bool {
	for _, name := range []string{storeDir, readmeFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !isSystemFile(e.Name()) {
			return false
		}
	}
	return true
}

// appsRoot returns the folder of the apps' folders in the data folder
// dataDir: WSS-Data/apps.
func appsRoot(dataDir string) string {
	return filepath.Join(wssDir(dataDir), storeDir)
}

// ensureWSS creates the WSS-Data folder wss when it is missing, and its
// README.txt; an existing README.txt is never written over.
func ensureWSS(wss string) error {
	if err := os.Mkdir(wss, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if fi, err := os.Stat(wss); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", wss)
	}
	path := filepath.Join(wss, readmeFile)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return err // there already, or cannot be told
	}
	return fsutil.WriteFileAtomic(path, []byte(readmeText), 0o644)
}

// removeWSSIfEmpty deletes the WSS-Data folder wss and its README.txt when
// nothing else is left in it. A WSS-Data that is a link (a symbolic link, or
// a junction on Windows) loses only its README.txt: the link is the user's,
// and a restart would make a local folder in its place. A missing folder is
// nothing to do.
func removeWSSIfEmpty(wss string) error {
	entries, err := os.ReadDir(wss)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != readmeFile {
			return nil
		}
	}
	if err := os.Remove(filepath.Join(wss, readmeFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if isLink(wss) {
		return nil
	}
	return os.Remove(wss)
}

// isLink reports whether path is a symbolic link or another reparse point,
// such as a Windows junction, rather than a plain folder.
func isLink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && (fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir())
}

// checkDataDir tells what moving the data folder from current to target
// would do: both are data folders as the user chose them, and the verdict
// looks at their WSS-Data folders. It changes nothing but a probe file that
// tests whether target is writable. The error is for a failure to read the
// current folder; every problem with target is a verdict.
func checkDataDir(current, target string) (dataCheck, error) {
	c := dataCheck{Path: target, curWSS: wssDir(current)}
	var err error
	if c.curData, err = hasData(c.curWSS); err != nil {
		return c, err
	}
	if c.Current, err = listAppData(c.curWSS); err != nil {
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
	curWSS := c.curWSS
	c.tgtWSS = wssDir(target)
	tgtWSS := c.tgtWSS
	if samePath(target, current) || samePath(tgtWSS, curWSS) ||
		os.SameFile(statOrNil(target), statOrNil(current)) || os.SameFile(statOrNil(tgtWSS), statOrNil(curWSS)) {
		c.Verdict = verdictSame
		return c, nil
	}
	// One WSS-Data inside the other, or the target's being the current data
	// folder (a WSS-Data chosen itself that holds the data in a WSS-Data of
	// its own): a copy would go into itself, and deleting the old one would
	// delete the new.
	if within(curWSS, tgtWSS) || within(tgtWSS, curWSS) || samePath(tgtWSS, current) {
		return refuse("%s would keep the data in %s, which overlaps the current data in %s", target, tgtWSS, curWSS)
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
	if err := probeWritable(target); err != nil {
		return refuse("cannot write to %s: %v", target, err)
	}
	if wfi, err := os.Stat(tgtWSS); err == nil && !wfi.IsDir() {
		return refuse("%s is not a folder", tgtWSS)
	} else if err == nil {
		if err := probeWritable(tgtWSS); err != nil {
			return refuse("cannot write to %s: %v", tgtWSS, err)
		}
	}
	tgtData, err := hasData(tgtWSS)
	if err != nil {
		return refuse("cannot read %s: %v", tgtWSS, err)
	}
	if c.Target, err = listAppData(tgtWSS); err != nil {
		return refuse("cannot read %s: %v", tgtWSS, err)
	}
	switch {
	case c.curData && tgtData:
		c.Verdict, c.Message = verdictConflict, target+" already holds data of a workspace-server"
	case tgtData:
		c.Verdict = verdictAdopt
	case c.curData:
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

// samePath reports whether a and b are one path, ignoring case where
// filepath.Rel does (Windows).
func samePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
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

// The apps folder in WSS-Data may hold more than the server's: someone may
// have put a folder there by hand, and a folder the user chose that is
// itself named WSS-Data may have held an apps folder of its own. The
// server's own app folders are those with a valid app id that hold a
// document (data.json) or a history folder; everything else under apps is
// not data, and is never moved or deleted.
//
// The functions below take a WSS-Data folder (wss), resolved by the caller
// with wssDir.

// appDirs returns the names of the server's app folders in wss, sorted.
func appDirs(wss string) ([]string, error) {
	root := filepath.Join(wss, storeDir)
	entries, err := os.ReadDir(root)
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
		dir := filepath.Join(root, e.Name())
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

// hasData reports whether wss holds data: a document or a history version
// in one of the server's app folders. Empty app folders, and anything that
// is not the server's, are not data.
func hasData(wss string) (bool, error) {
	ids, err := appDirs(wss)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		has, err := hasFiles(filepath.Join(wss, storeDir, id))
		if err != nil || has {
			return has, err
		}
	}
	return false, nil
}

// removeApps deletes the server's app folders in wss, and the empty folders
// with a valid app id, then the apps folder if nothing else is left in it,
// and then README.txt and wss if nothing else is left in that (see
// removeWSSIfEmpty). Anything else stays.
func removeApps(wss string) error {
	root := filepath.Join(wss, storeDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return removeWSSIfEmpty(wss)
	}
	if err != nil {
		return err
	}
	ours, err := appDirs(wss)
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
	if err := removeWSSIfEmpty(wss); err != nil && first == nil {
		first = err
	}
	return first
}

// listAppData describes the apps with a valid id in wss that hold data.
func listAppData(wss string) ([]appData, error) {
	st := store.New(filepath.Join(wss, storeDir), time.Now)
	var out []appData
	for _, id := range adoptableApps(wss) {
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

// adoptableApps returns the ids of the server's app folders in wss, sorted.
// A missing folder has none.
func adoptableApps(wss string) []string {
	ids, _ := appDirs(wss)
	return ids
}

// moveData copies the server's app folders of the WSS-Data folder from into
// the WSS-Data folder to: first to to/apps.partial, fsynced, then into
// place, so that to never holds a half copy under the real name. An apps
// folder already in to must hold no data; its empty app folders are
// replaced, and anything that is not the server's stays. The source is left
// in place: the caller deletes it once the configuration points at to. A
// source with no data, and two folders one inside the other, are errors. On
// a failure nothing is added to to, and to itself goes again if moveData
// made it. README.txt is the caller's.
func moveData(from, to string) error {
	if within(from, to) || within(to, from) {
		return fmt.Errorf("%s and %s overlap", from, to)
	}
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
		return fmt.Errorf("%s holds no data to move", from)
	}
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	_, statErr := os.Stat(to)
	madeWSS := errors.Is(statErr, os.ErrNotExist)
	fail := func(err error) error {
		_ = os.RemoveAll(partial)
		if madeWSS {
			_ = removeWSSIfEmpty(to)
		}
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
		_ = fsutil.SyncDir(filepath.Dir(to)) // a WSS-Data folder made here
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
// and the server's app folders in the data folder dataDir (see removeApps).
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
	if err := removeApps(wssDir(dataDir)); err != nil && first == nil {
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
// folder: the server's app folders and the WSS-Data folder (see removeApps),
// and then the folder itself only if nothing else is left in it. The user
// may have chosen a folder that holds other files. A missing folder is
// nothing to do.
func removeData(dataDir string) error {
	return removeDataIn(dataDir, wssDir(dataDir))
}

// removeDataIn is removeData with the WSS-Data folder wss already resolved.
func removeDataIn(dataDir, wss string) error {
	if err := removeApps(wss); err != nil {
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
