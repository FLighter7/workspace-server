package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// devVersion is the version of a build made without -X main.version: any
// plain go build or go run.
const devVersion = "dev"

// devBuildError is the refusal of a development build to use a folder of the
// installed server.
type devBuildError struct{ dir string }

func (e devBuildError) Error() string {
	return fmt.Sprintf("this is a development build (version %s): it does not use %s, a folder of the installed server. "+
		"Run it with -dir tmp/dev, or build it with a version (-ldflags \"-X main.version=<v>\")", devVersion, e.dir)
}

// errDevStart is start's refusal in a development build.
var errDevStart = fmt.Errorf("this is a development build (version %s): it is never started at login. "+
	"Use a release build, or build it with a version (-ldflags \"-X main.version=<v>\")", devVersion)

// checkDevDirs returns a devBuildError when v is a development build and one
// of dirs is the default server folder def, the data folder def's config
// names, or a folder inside either. A release build, or an empty def, passes
// anything. dirs are absolute paths; empty ones are skipped.
func checkDevDirs(v, def string, dirs ...string) error {
	if v != devVersion || def == "" {
		return nil
	}
	protected := []string{def}
	if data := readDataDir(def); filepath.Clean(data) != filepath.Clean(def) {
		protected = append(protected, data)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, p := range protected {
			if within(p, dir) || os.SameFile(statOrNil(p), statOrNil(dir)) {
				return devBuildError{dir}
			}
		}
	}
	return nil
}

// checkDevDirsHere is checkDevDirs for this build and this user's default
// server folder. A default folder that cannot be found protects nothing.
func checkDevDirsHere(dirs ...string) error {
	def, err := defaultServerDir()
	if err != nil {
		return nil
	}
	return checkDevDirs(version, def, dirs...)
}
