package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	helperOnce sync.Once
	helperBin  string
	helperErr  error
)

// helperPath builds the helper dev server in testdata/helper and returns the
// path of the binary. By default the helper ignores PORT, like Vite.
func helperPath(t testing.TB) string {
	t.Helper()
	helperOnce.Do(func() {
		var dir string
		dir, helperErr = os.MkdirTemp("", "doze-helper")
		if helperErr != nil {
			return
		}
		helperBin = filepath.Join(dir, "helper")
		_, file, _, _ := runtime.Caller(0)
		cmd := exec.Command("go", "build", "-o", helperBin, ".")
		cmd.Dir = filepath.Join(filepath.Dir(file), "testdata", "helper")
		var out []byte
		out, helperErr = cmd.CombinedOutput()
		if helperErr != nil {
			helperErr = &buildError{helperErr, out}
		}
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	return helperBin
}

type buildError struct {
	err error
	out []byte
}

func (e *buildError) Error() string { return e.err.Error() + ": " + string(e.out) }

// shortTempDir returns a short temporary directory. Unix socket paths on macOS
// must be less than 104 bytes, and t.TempDir paths are often longer.
func shortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "doze")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
