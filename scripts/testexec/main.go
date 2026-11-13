// Command testexec runs a Go test binary from a fixed path, for go test -exec.
//
// go test builds every test binary in a new temporary folder, so Windows
// Firewall asks about each one again on every run of a test that listens on
// the network. testexec copies the binary to tmp/testbin/ next to itself,
// named after the package's folder, and runs it from there: the firewall then
// asks once per package, or never once a rule allows that path.
//
//	go build -o tmp/testexec.exe ./scripts/testexec
//	go test -exec "$PWD/tmp/testexec.exe" ./...
//
// When the fixed copy cannot be made (another run of the same package holds
// it), the binary runs where go test built it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testexec <test binary> [arguments]")
		os.Exit(2)
	}
	bin := os.Args[1]
	run := bin
	if fixed, err := pin(bin); err != nil {
		fmt.Fprintf(os.Stderr, "testexec: %v; running %s in place\n", err, bin)
	} else {
		run = fixed
	}

	cmd := exec.Command(run, os.Args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if code < 0 {
			code = 1
		}
		os.Exit(code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testexec:", err)
		os.Exit(1)
	}
}

// pin copies bin to testbin/<name>-<package hash><ext> next to this program
// and returns that path. go test runs the binary in the package's folder, so
// the working directory tells two packages with the same name apart.
func pin(bin string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(wd))))
	ext := filepath.Ext(bin)
	name := strings.TrimSuffix(filepath.Base(bin), ext)
	dir := filepath.Join(filepath.Dir(self), "testbin")
	dst := filepath.Join(dir, name+"-"+hex.EncodeToString(sum[:4])+ext)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	src, err := os.Open(bin)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, name+"-*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}
