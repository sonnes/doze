package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func start(t *testing.T, args ...string) (*Process, int, *syncBuffer) {
	t.Helper()
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	p, err := StartProcess(ProcessSpec{
		Cmd:      append([]string{helperPath(t)}, args...),
		Port:     port,
		Stdout:   out,
		Stderr:   out,
		NewGroup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(time.Second) })
	return p, port, out
}

func TestWaitPortFindsServerThatIgnoresPORT(t *testing.T) {
	p, port, out := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := WaitPort(ctx, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got == port {
		t.Fatalf("helper ignores PORT, but WaitPort returned PORT %d", port)
	}
	if !strings.Contains(out.String(), ":"+strconv.Itoa(got)) {
		t.Fatalf("WaitPort = %d, helper output %q", got, out.String())
	}
}

func TestWaitPortWithKnownPortWaitsForDial(t *testing.T) {
	p, port, _ := start(t, "-port-env")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := WaitPort(ctx, p, port)
	if err != nil {
		t.Fatal(err)
	}
	if got != port {
		t.Fatalf("WaitPort = %d, want %d", got, port)
	}
}

func TestWaitPortFailsWhenProcessExits(t *testing.T) {
	p, _, _ := start(t, "-exit", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := WaitPort(ctx, p, 0); err == nil {
		t.Fatal("WaitPort returned no error for a process that exited")
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	// sh starts the helper as a child, so the helper is only reachable
	// through the process group.
	port, _ := FreePort()
	p, err := StartProcess(ProcessSpec{
		Cmd:      []string{"sh", "-c", helperPath(t) + " -port-env; sleep 60"},
		Port:     port,
		NewGroup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := WaitPort(ctx, p, port); err != nil {
		t.Fatal(err)
	}

	if err := p.Stop(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("process is not done after Stop")
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
		c.Close()
		t.Fatal("child of the process still listens after Stop")
	}
}

func TestExpandReplacesPortPlaceholder(t *testing.T) {
	got, ok := Expand([]string{"go", "run", ".", "-addr", ":{port}"}, 4321)
	if !ok {
		t.Fatal("Expand reported no placeholder")
	}
	if want := "go run . -addr :4321"; strings.Join(got, " ") != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
	if _, ok := Expand([]string{"pnpm", "dev"}, 4321); ok {
		t.Fatal("Expand reported a placeholder in a command without one")
	}
}

func TestStartRunsSingleStringThroughShell(t *testing.T) {
	out := &syncBuffer{}
	p, err := StartProcess(ProcessSpec{Cmd: []string{"echo $PORT"}, Port: 4321, Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if got := strings.TrimSpace(out.String()); got != "4321" {
		t.Fatalf("output = %q, want 4321", got)
	}
}

func TestStartFindsCommandInPATHOfSpec(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho found\n"
	if err := os.WriteFile(filepath.Join(dir, "doze-test-cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	p, err := StartProcess(ProcessSpec{
		Cmd:    []string{"doze-test-cmd"},
		Env:    []string{"PATH=" + dir + ":/usr/bin:/bin"},
		Stdout: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if got := strings.TrimSpace(out.String()); got != "found" {
		t.Fatalf("output = %q, want found", got)
	}
}
