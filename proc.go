// This file starts dev servers, finds the port that they listen on, and
// stops them with their child processes.

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcessSpec describes a command to start.
type ProcessSpec struct {
	Dir  string
	Cmd  []string
	Env  []string // added to the environment of this process
	Port int      // the value of PORT and {port}

	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// NewGroup puts the command in its own process group. Stop then signals
	// the whole group. Without it, the command shares the group of this
	// process, so the terminal sends Ctrl-C to it.
	NewGroup bool
}

// Process is a started command.
type Process struct {
	Port int // the value of PORT
	Pgid int

	cmd   *exec.Cmd
	group bool
	done  chan struct{}
	err   error
}

// Expand replaces each {port} in cmd with port. It reports whether cmd had
// a placeholder.
func Expand(cmd []string, port int) ([]string, bool) {
	out := make([]string, len(cmd))
	found := false
	for i, a := range cmd {
		if strings.Contains(a, "{port}") {
			found = true
			a = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
		}
		out[i] = a
	}
	return out, found
}

// lookPath finds name in the last PATH of env. exec.Command uses the PATH
// of the daemon, and launchd gives the daemon a short PATH. If name has a
// slash, or no absolute directory in PATH holds it, lookPath returns name.
func lookPath(name string, env []string) string {
	if strings.Contains(name, "/") {
		return name
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		file := filepath.Join(dir, name)
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		return file
	}
	return name
}

// StartProcess starts the command in s. A command of one string with spaces or
// shell characters runs through sh -c.
func StartProcess(s ProcessSpec) (*Process, error) {
	if len(s.Cmd) == 0 {
		return nil, errors.New("no command")
	}
	args, _ := Expand(s.Cmd, s.Port)
	if len(args) == 1 && strings.ContainsAny(args[0], " \t$|&;<>()'\"") {
		args = []string{"sh", "-c", args[0]}
	}

	env := append(os.Environ(), s.Env...)
	env = append(env, "PORT="+strconv.Itoa(s.Port))

	cmd := exec.Command(lookPath(args[0], env), args[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	if s.NewGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &Process{Port: s.Port, cmd: cmd, group: s.NewGroup, done: make(chan struct{})}
	if s.NewGroup {
		p.Pgid = cmd.Process.Pid
	} else {
		p.Pgid = syscall.Getpgrp()
	}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Done is closed when the process exits.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the exit error after Done is closed.
func (p *Process) Err() error { return p.err }

// ExitCode returns the exit code after Done is closed. A process that a
// signal stopped gets 128 plus the signal number, as in a shell.
func (p *Process) ExitCode() int {
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return p.cmd.ProcessState.ExitCode()
}

// Signal sends sig to the process, or to its group if it has its own.
func (p *Process) Signal(sig syscall.Signal) error {
	if p.group {
		return syscall.Kill(-p.Pgid, sig)
	}
	return p.cmd.Process.Signal(sig)
}

// Stop sends SIGTERM. If the process is still running after grace, Stop
// sends SIGKILL. With its own group, the signals go to every process in it.
func (p *Process) Stop(grace time.Duration) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	p.Signal(syscall.SIGTERM)
	kill := time.After(grace)
	var giveUp <-chan time.Time
	for !p.gone() {
		select {
		case <-kill:
			p.Signal(syscall.SIGKILL)
			giveUp = time.After(2 * time.Second)
		case <-giveUp:
			return fmt.Errorf("process group %d is still running after SIGKILL", p.Pgid)
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// gone reports whether the process has exited and, with its own group,
// whether every process in the group has exited.
func (p *Process) gone() bool {
	select {
	case <-p.done:
	default:
		return false
	}
	return !p.group || syscall.Kill(-p.Pgid, 0) == syscall.ESRCH
}

// WaitPort waits until the process serves HTTP and returns the port. If
// known is not 0, WaitPort waits for a connection to that port. Otherwise it
// finds a TCP port that a process in the group listens on, and prefers PORT.
func WaitPort(ctx context.Context, p *Process, known int) (int, error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if known != 0 {
			if Dial(known) {
				return known, nil
			}
		} else {
			ports, err := ListeningPorts(p.Pgid)
			if err != nil {
				return 0, err
			}
			if len(ports) > 0 {
				if slices.Contains(ports, p.Port) {
					return p.Port, nil
				}
				return ports[0], nil
			}
		}
		select {
		case <-p.done:
			return 0, fmt.Errorf("process exited: %v", p.err)
		case <-ctx.Done():
			return 0, fmt.Errorf("no port after %v", ctx.Err())
		case <-tick.C:
		}
	}
}

// Dial reports whether a TCP connection to localhost:port succeeds.
func Dial(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ListeningPorts returns the sorted TCP ports that processes in the group
// pgid listen on.
func ListeningPorts(pgid int) ([]int, error) {
	out, err := exec.Command("lsof", "-nP", "-a", "-g", strconv.Itoa(pgid),
		"-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(out) == 0 {
		return nil, nil // lsof exits 1 when it finds nothing
	}
	if err != nil {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	var ports []int
	s := bufio.NewScanner(bytes.NewReader(out))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}
		i := strings.LastIndexByte(line, ':')
		port, err := strconv.Atoi(line[i+1:])
		if err == nil && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return ports, nil
}

// FreePort returns a TCP port that is free now.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
