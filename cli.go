package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const usage = `doze gives each dev server a <name>.localhost URL.

Usage:
  doze [flags] <command...>          run a command and route <name>.localhost to it
  doze run [flags] <command...>      the same, for a command named like a subcommand
  doze register [flags] <command...> save a command; it starts on the first request
  doze unregister <name>
  doze ls
  doze start <name>
  doze stop <name>
  doze logs [-f] <name>
  doze hosts                         write a line to /etc/hosts for each app with a domain
  doze daemon [stop]
  doze setup [--domain <domain>]     trust the HTTPS CA and start the daemon at login (macOS)
  doze uninstall                     undo doze setup
  doze version

Flags:
  --name <name>   the subdomain (default: the package.json name or the directory name)
  --port <port>   the port the app listens on, if doze must not find it
  --idle <time>   stop a registered app after this idle time (default 30m, 0 = never)
  --domain <d>    also route <name>.<d>. Run doze setup --domain <d> once for HTTPS.

A command can use {port}, for example: doze register go run . -addr :{port}
Every command also gets the PORT environment variable.

Environment:
  DOZE_HOME         state directory (default ~/.local/state/doze)
  DOZE_ADDR         HTTP address of the proxy (default :80)
  DOZE_HTTPS_ADDR   HTTPS address of the proxy (default :443)
  DOZE_HOSTS_FILE   the hosts file that doze updates (default /etc/hosts)
`

// Main runs the doze command and returns the exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "version", "--version":
		fmt.Println("doze", version())
		return 0
	case "run":
		return runOneOff(args[1:])
	case "register":
		err = register(args[1:])
	case "unregister", "start", "stop":
		err = byName(args[0], args[1:])
	case "ls":
		err = list()
	case "logs":
		err = logs(args[1:])
	case "hosts":
		err = hosts()
	case "daemon":
		if len(args) > 1 && args[1] == "stop" {
			err = stopDaemon()
		} else {
			err = runDaemon()
		}
	case "setup":
		err = setup(args[1:])
	case "uninstall":
		err = uninstall()
	default:
		return runOneOff(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 1
	}
	return 0
}

// version returns the module version from the build info. go install and
// go build in a tagged checkout set it, so a release needs no -ldflags.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

func home() string {
	if h := os.Getenv("DOZE_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "doze")
}

func sockPath() string { return filepath.Join(home(), "doze.sock") }

type options struct {
	name   string
	port   int
	idle   time.Duration
	domain string
	cmd    []string
}

func parse(name string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.name, "name", "", "")
	fs.IntVar(&o.port, "port", 0, "")
	fs.DurationVar(&o.idle, "idle", 30*time.Minute, "")
	fs.StringVar(&o.domain, "domain", "", "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.cmd = fs.Args()
	o.domain = strings.ToLower(o.domain)
	if err := validDomain(o.domain); err != nil {
		return o, err
	}
	if o.name == "" {
		dir, _ := os.Getwd()
		o.name = InferName(dir)
	}
	return o, ValidName(o.name)
}

// InferName returns the package.json name of dir without its scope, or the
// name of dir. The result has only lowercase letters, digits, and hyphens.
func InferName(dir string) string {
	name := filepath.Base(dir)
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct{ Name string }
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			name = pkg.Name[strings.LastIndexByte(pkg.Name, '/')+1:]
		}
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// appURL returns the HTTPS URL of host when the daemon serves HTTPS, and the
// HTTP URL otherwise.
func appURL(host string, httpPort, httpsPort int) string {
	scheme, port, defaultPort := "http", httpPort, 80
	if httpsPort != 0 {
		scheme, port, defaultPort = "https", httpsPort, 443
	}
	if port == defaultPort {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + strconv.Itoa(port)
}

// appURLs returns the URL of name under localhost, and under domain if it is
// not empty, separated by two spaces.
func appURLs(name, domain string, httpPort, httpsPort int) string {
	urls := appURL(name+".localhost", httpPort, httpsPort)
	if domain != "" {
		urls += "  " + appURL(name+"."+domain, httpPort, httpsPort)
	}
	return urls
}

// warnDomain reports a domain that the CA does not permit. The app works
// over HTTP, so the command goes on.
func warnDomain(domain string) {
	if domain == "" {
		return
	}
	if ca, err := LoadCA(home()); err == nil && !ca.Permits(domain) {
		fmt.Fprintf(os.Stderr, "doze: no HTTPS for %s. Run doze setup --domain %s\n", domain, domain)
	}
}

// warnHosts reports a failed update of the hosts file. The route works
// without it on localhost, so the failure does not stop the command.
func warnHosts(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "doze: %s is not updated: %v. Run doze hosts in a terminal\n", hostsFile(), err)
	}
}

// connect returns a client. It starts the daemon in the background if it
// does not answer. When commands start at the same time, only one daemon
// can listen and the others exit. So an exit is not a failure until the
// wait ends.
func connect() (*Client, error) {
	c := NewClient(sockPath())
	if c.Ping() {
		return c, nil
	}
	if err := os.MkdirAll(home(), 0o700); err != nil {
		return nil, err
	}
	logPath := filepath.Join(home(), "daemon.log")
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = home()
	cmd.Env = daemonEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	for range 100 {
		if c.Ping() {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("the daemon did not answer. See %s", logPath)
}

// daemonEnv returns the variables of env that a daemon gets from launchd,
// and the DOZE_ variables. Registered apps get the environment of the
// daemon. So without this filter, they get the environment of the shell that
// started the daemon, and that shell can hold secrets.
func daemonEnv(env []string) []string {
	keep := []string{"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "PATH", "LANG"}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keep, k) || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "DOZE_") {
			out = append(out, kv)
		}
	}
	return out
}

func runOneOff(args []string) int {
	o, err := parse("doze", args)
	if err == nil && len(o.cmd) == 0 {
		err = errors.New("no command. Run doze --help")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 2
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 1
	}
	a, err := c.Attach(o.name, o.domain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 1
	}
	defer a.Close()
	warnHosts(syncHosts(c))
	warnDomain(o.domain)

	free, err := FreePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 1
	}
	// A shell job leader shares its process group with the command, so the
	// terminal sends Ctrl-C to both. Otherwise the command gets its own group
	// so that port discovery sees only its processes.
	leader := syscall.Getpgrp() == os.Getpid()
	p, err := StartProcess(ProcessSpec{
		Cmd: o.cmd, Port: free, NewGroup: !leader,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "doze:", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			if s == syscall.SIGINT && leader {
				continue // the terminal sent it to the command too
			}
			p.Signal(s.(syscall.Signal))
		}
	}()

	go func() {
		known := o.port
		if _, ok := Expand(o.cmd, free); ok && known == 0 {
			known = free
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		port, err := WaitPort(ctx, p, known)
		if err != nil {
			select {
			case <-p.Done():
			default:
				fmt.Fprintf(os.Stderr, "doze: %s: %v\n", o.name, err)
			}
			return
		}
		a.SetPort(port)
		fmt.Fprintf(os.Stderr, "doze  %s  %s  -> localhost:%d\n", o.name, appURLs(o.name, o.domain, a.ProxyPort, a.HTTPSPort), port)
	}()

	<-p.Done()
	return p.ExitCode()
}

func register(args []string) error {
	o, err := parse("register", args)
	if err != nil {
		return err
	}
	if len(o.cmd) == 0 && o.port == 0 {
		return errors.New("give a command, or --port for a server that doze does not start")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	app := App{
		Name: o.name, Dir: dir, Cmd: o.cmd, Port: o.port, Idle: Duration(o.idle), Domain: o.domain,
		Env: []string{"PATH=" + os.Getenv("PATH")}, // launchd gives the daemon a short PATH
	}
	if err := c.Register(app); err != nil {
		return err
	}
	warnHosts(syncHosts(c))
	warnDomain(o.domain)
	hp, sp, _ := c.Ports()
	fmt.Printf("doze  %s  %s  starts on the first request\n", o.name, appURLs(o.name, o.domain, hp, sp))
	return nil
}

// hosts writes a <name>.<domain> line to the hosts file for each app with a
// domain.
func hosts() error {
	c, err := connect()
	if err != nil {
		return err
	}
	return syncHosts(c)
}

func byName(op string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: doze %s <name>", op)
	}
	c, err := connect()
	if err != nil {
		return err
	}
	switch op {
	case "unregister":
		return c.Unregister(args[0])
	case "start":
		return c.Start(args[0])
	default:
		return c.Stop(args[0])
	}
}

func list() error {
	c, err := connect()
	if err != nil {
		return err
	}
	apps, err := c.List()
	if err != nil {
		return err
	}
	hp, sp, _ := c.Ports()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tURL\tPORT\tCOMMAND")
	for _, a := range apps {
		port, cmd := "-", strings.Join(a.Cmd, " ")
		if a.Port != 0 {
			port = strconv.Itoa(a.Port)
		}
		if a.OneOff {
			cmd = "(one-off run)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.State, appURL(a.Name+".localhost", hp, sp), port, cmd)
	}
	return w.Flush()
}

func logs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: doze logs [-f] <name>")
	}
	path := LogFile(home(), fs.Arg(0))
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no logs for %s", fs.Arg(0))
	}
	defer f.Close()
	n, _ := io.Copy(os.Stdout, f)
	for *follow {
		time.Sleep(200 * time.Millisecond)
		if fi, err := os.Stat(path); err == nil && fi.Size() < n {
			// The app restarted and the file starts again.
			f.Close()
			if f, err = os.Open(path); err != nil {
				return err
			}
			n = 0
		}
		m, _ := io.Copy(os.Stdout, f)
		n += m
	}
	return nil
}

func runDaemon() error {
	h := home()
	// The control socket can start any command, so only the user can use it.
	if err := os.MkdirAll(h, 0o700); err != nil {
		return err
	}
	c := NewClient(sockPath())
	if c.Ping() {
		fmt.Fprintln(os.Stderr, "doze: the daemon is already running")
		return nil
	}
	addr := os.Getenv("DOZE_ADDR")
	if addr == "" {
		addr = ":80"
	}
	pl, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%w. Set DOZE_ADDR to another address, for example 127.0.0.1:7355", err)
	}
	ca, err := LoadCA(h)
	if err != nil {
		return err
	}
	httpsAddr := os.Getenv("DOZE_HTTPS_ADDR")
	if httpsAddr == "" {
		httpsAddr = ":443"
	}
	// Without HTTPS, the apps still work over HTTP. So a busy port is not fatal.
	tl, err := net.Listen("tcp", httpsAddr)
	httpsPort := 0
	if err != nil {
		fmt.Fprintf(os.Stderr, "doze: no HTTPS: %v. Set DOZE_HTTPS_ADDR to another address\n", err)
	} else {
		httpsPort = tl.Addr().(*net.TCPAddr).Port
	}

	d, err := NewDaemon(Config{
		Home:      h,
		ProxyPort: pl.Addr().(*net.TCPAddr).Port,
		HTTPSPort: httpsPort,
	})
	if err != nil {
		return err
	}
	os.Remove(sockPath())
	cl, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	if err := os.Chmod(sockPath(), 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: d.Handler()}
	go srv.Serve(LoopbackOnly(pl))
	tlsSrv := &http.Server{
		Handler:   d.Handler(),
		TLSConfig: ca.TLSConfig(),
	}
	if tl != nil {
		go tlsSrv.ServeTLS(LoopbackOnly(tl), "", "")
		fmt.Fprintf(os.Stderr, "doze: HTTPS on %s\n", tl.Addr())
	}
	go d.ServeControl(cl)
	fmt.Fprintf(os.Stderr, "doze: daemon on %s\n", pl.Addr())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigs:
	case <-d.Quit():
	}
	cl.Close()
	os.Remove(sockPath())
	srv.Close()
	tlsSrv.Close()
	d.Shutdown()
	return nil
}

func stopDaemon() error {
	c := NewClient(sockPath())
	if !c.Ping() {
		return nil
	}
	if err := c.Shutdown(); err != nil {
		return err
	}
	for range 100 {
		if !c.Ping() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the daemon did not stop")
}
