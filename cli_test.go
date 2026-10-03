package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInferName(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	os.Mkdir(web, 0o755)
	os.WriteFile(filepath.Join(web, "package.json"), []byte(`{"name": "@september/Web_App"}`), 0o644)
	if got := InferName(web); got != "web-app" {
		t.Errorf("InferName with package.json = %q, want web-app", got)
	}

	api := filepath.Join(dir, "My API")
	os.Mkdir(api, 0o755)
	if got := InferName(api); got != "my-api" {
		t.Errorf("InferName without package.json = %q, want my-api", got)
	}
}

var dozeBin string

func build(t *testing.T) string {
	t.Helper()
	if dozeBin != "" {
		return dozeBin
	}
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(shortTempDir(t), "doze")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(filepath.Dir(file))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Keep the binary for later tests in a directory that outlives this one.
	keep, _ := os.MkdirTemp("", "doze-bin")
	dozeBin = filepath.Join(keep, "doze")
	os.Rename(bin, dozeBin)
	return dozeBin
}

type env struct {
	t    *testing.T
	bin  string
	home string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, bin: build(t), home: shortTempDir(t)}
	t.Cleanup(func() { NewClient(filepath.Join(e.home, "doze.sock")).Shutdown() })
	return e
}

func (e *env) cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(), "DOZE_HOME="+e.home, "DOZE_ADDR=127.0.0.1:0", "DOZE_HTTPS_ADDR=127.0.0.1:0")
	cmd.Dir = e.t.TempDir()
	return cmd
}

func (e *env) run(args ...string) string {
	e.t.Helper()
	out, err := e.cmd(args...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("doze %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (e *env) get(host string) (int, string) {
	e.t.Helper()
	port, _, err := NewClient(filepath.Join(e.home, "doze.sock")).Ports()
	if err != nil {
		e.t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/", nil)
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestRegisterStartsAppOnFirstRequest(t *testing.T) {
	e := newEnv(t)
	out := e.run("register", "--name", "api", helperPath(t))
	if !strings.Contains(out, "api.localhost") {
		t.Fatalf("register output = %q", out)
	}
	if ls := e.run("ls"); !strings.Contains(ls, "api") || !strings.Contains(ls, "stopped") {
		t.Fatalf("ls before request = %q", ls)
	}

	if code, body := e.get("api.localhost"); code != 200 || !strings.Contains(body, "host=api.localhost") {
		t.Fatalf("got %d %q", code, body)
	}
	if ls := e.run("ls"); !strings.Contains(ls, "running") {
		t.Fatalf("ls after request = %q", ls)
	}
	if logs := e.run("logs", "api"); !strings.Contains(logs, "listening on") {
		t.Fatalf("logs = %q", logs)
	}

	e.run("stop", "api")
	if ls := e.run("ls"); !strings.Contains(ls, "stopped") {
		t.Fatalf("ls after stop = %q", ls)
	}
	e.run("unregister", "api")
	if code, _ := e.get("api.localhost"); code != http.StatusNotFound {
		t.Fatalf("got %d after unregister, want 404", code)
	}
}

func TestOneOffRunRoutesUntilCtrlC(t *testing.T) {
	e := newEnv(t)
	cmd := e.cmd("--name", "tmp", helperPath(t))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like a shell job
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- cmd.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.home, "doze.sock")); err == nil {
			if code, body := e.get("tmp.localhost"); code == 200 {
				if !strings.Contains(body, "host=tmp.localhost") {
					t.Fatalf("body = %q", body)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("one-off run never served")
		}
		time.Sleep(50 * time.Millisecond)
	}

	syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) // what the terminal does on Ctrl-C
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("doze did not exit after Ctrl-C")
	}
	if code, _ := e.get("tmp.localhost"); code != http.StatusNotFound {
		t.Fatalf("got %d after exit, want 404", code)
	}
}

func TestOneOffRunReturnsExitCode(t *testing.T) {
	e := newEnv(t)
	err := e.cmd("--name", "bad", helperPath(t), "-exit", "3").Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit code 3", err)
	}
}

func TestRegisterPrintsHTTPSURLThatServes(t *testing.T) {
	e := newEnv(t)
	out := e.run("register", "--name", "api", helperPath(t))
	_, rest, ok := strings.Cut(out, "https://api.localhost:")
	if !ok {
		t.Fatalf("register output = %q, want an https URL", out)
	}
	port, _, _ := strings.Cut(rest, " ")

	ca, err := LoadCA(e.home)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://127.0.0.1:"+port+"/", nil)
	req.Host = "api.localhost"
	res, err := httpsClient(ca, "api.localhost").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "host=api.localhost") {
		t.Fatalf("got %d %q", res.StatusCode, body)
	}
}

func TestConcurrentCommandsShareOneDaemon(t *testing.T) {
	e := newEnv(t)
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	// With a fixed address, only one daemon can listen. The other commands
	// must use that daemon.
	errs := make(chan error)
	for range 4 {
		go func() {
			cmd := e.cmd("ls")
			cmd.Env = append(cmd.Env, "DOZE_ADDR=127.0.0.1:"+strconv.Itoa(port))
			out, err := cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%v: %s", err, out)
			}
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestRegisteredAppDoesNotGetShellEnvironment(t *testing.T) {
	e := newEnv(t)
	// This command starts the daemon. A registered app must not get the
	// environment of the shell that started it.
	cmd := e.cmd("register", "--name", "api", helperPath(t), "-env", "SECRET_TOKEN")
	cmd.Env = append(cmd.Env, "SECRET_TOKEN=leak")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}
	code, body := e.get("api.localhost")
	if code != 200 || strings.Contains(body, "leak") {
		t.Fatalf("got %d %q, want 200 without the shell environment", code, body)
	}
}

func TestControlSocketIsPrivate(t *testing.T) {
	e := newEnv(t)
	e.run("ls")
	fi, err := os.Stat(filepath.Join(e.home, "doze.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}
