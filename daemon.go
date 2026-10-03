// The daemon keeps the route table. It starts registered apps on their
// first request, stops them when idle, and routes one-off runs.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// App is a registered app.
type App struct {
	Name string   `json:"name"`
	Dir  string   `json:"dir"`
	Cmd  []string `json:"cmd,omitempty"`
	Env  []string `json:"env,omitempty"`  // for example the PATH at registration
	Port int      `json:"port,omitempty"` // a fixed upstream port
	Idle Duration `json:"idle"`           // 0 keeps the app running
}

// Duration is a time.Duration that is a string in JSON, such as "30m".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Status is the state of one route.
type Status struct {
	Name   string   `json:"name"`
	State  string   `json:"state"` // stopped, starting, running, crashed
	Port   int      `json:"port,omitempty"`
	OneOff bool     `json:"one_off,omitempty"`
	Dir    string   `json:"dir,omitempty"`
	Cmd    []string `json:"cmd,omitempty"`
	Idle   Duration `json:"idle"`
	Error  string   `json:"error,omitempty"`
}

const (
	stopped  = "stopped"
	starting = "starting"
	running  = "running"
	crashed  = "crashed"
)

// Config configures a Daemon.
type Config struct {
	Home         string        // holds apps.json and logs/
	StartTimeout time.Duration // the time an app has to open its port
	StopGrace    time.Duration // the time between SIGTERM and SIGKILL
	ProxyPort    int           // the port of the proxy, for URLs
	HTTPSPort    int           // the HTTPS port of the proxy, for URLs. 0 means no HTTPS.
}

// ErrNotFound means that no route has the name.
var ErrNotFound = errors.New("no app with this name")

// Daemon owns the route table.
type Daemon struct {
	cfg Config

	mu     sync.Mutex
	routes map[string]*route

	quit     chan struct{}
	quitOnce sync.Once
}

type route struct {
	name string
	app  *App // nil for a one-off run without a registration

	state    string
	port     int
	proc     *Process
	ready    chan struct{} // closed when the current start finishes
	err      error
	gen      int // changes on each start and stop
	active   int // open requests and connections
	idleStop *time.Timer

	oneoff *oneoff
}

type oneoff struct {
	port  int
	ready chan struct{}
}

// NewDaemon loads the registered apps from cfg.Home.
func NewDaemon(cfg Config) (*Daemon, error) {
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if err := os.MkdirAll(filepath.Join(cfg.Home, "logs"), 0o755); err != nil {
		return nil, err
	}
	d := &Daemon{cfg: cfg, routes: map[string]*route{}, quit: make(chan struct{})}
	data, err := os.ReadFile(d.appsFile())
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	var apps []App
	if err := json.Unmarshal(data, &apps); err != nil {
		return nil, fmt.Errorf("%s: %w", d.appsFile(), err)
	}
	for _, a := range apps {
		d.routes[a.Name] = &route{name: a.Name, app: &a, state: stopped}
	}
	return d, nil
}

func (d *Daemon) appsFile() string { return filepath.Join(d.cfg.Home, "apps.json") }

// LogFile returns the log file of a registered app.
func LogFile(home, name string) string { return filepath.Join(home, "logs", name+".log") }

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// ValidName reports whether name can be a subdomain of localhost.
func ValidName(name string) error {
	if !nameRE.MatchString(name) || name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return fmt.Errorf("bad name %q: use lowercase letters, digits, and hyphens", name)
	}
	return nil
}

// Register adds or replaces a registered app. A running app with the same
// name stops.
func (d *Daemon) Register(a App) error {
	if err := ValidName(a.Name); err != nil {
		return err
	}
	if len(a.Cmd) == 0 && a.Port == 0 {
		return errors.New("an app needs a command or a port")
	}
	d.mu.Lock()
	r := d.routes[a.Name]
	if r == nil {
		r = &route{name: a.Name, state: stopped}
		d.routes[a.Name] = r
	}
	p := d.stopLocked(r)
	r.app = &a
	err := d.saveLocked()
	d.mu.Unlock()
	d.stopProc(p)
	return err
}

// Unregister stops an app and removes its registration.
func (d *Daemon) Unregister(name string) error {
	d.mu.Lock()
	r := d.routes[name]
	if r == nil || r.app == nil {
		d.mu.Unlock()
		return ErrNotFound
	}
	p := d.stopLocked(r)
	r.app = nil
	if r.oneoff == nil {
		delete(d.routes, name)
	}
	err := d.saveLocked()
	d.mu.Unlock()
	d.stopProc(p)
	return err
}

func (d *Daemon) saveLocked() error {
	apps := []App{}
	for _, r := range d.routes {
		if r.app != nil {
			apps = append(apps, *r.app)
		}
	}
	slices.SortFunc(apps, func(a, b App) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.appsFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.appsFile())
}

// List returns the status of each route, sorted by name.
func (d *Daemon) List() []Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Status{}
	for _, r := range d.routes {
		s := Status{Name: r.name, State: r.state, Port: r.port}
		if r.app != nil {
			s.Dir, s.Cmd, s.Idle = r.app.Dir, r.app.Cmd, r.app.Idle
			if len(r.app.Cmd) == 0 {
				s.State, s.Port = running, r.app.Port
			}
		}
		if r.err != nil && r.state == crashed {
			s.Error = r.err.Error()
		}
		if o := r.oneoff; o != nil {
			s.OneOff, s.Port, s.State = true, o.port, running
			if o.port == 0 {
				s.State = starting
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Start starts a registered app and waits until it serves.
func (d *Daemon) Start(ctx context.Context, name string) error {
	_, release, err := d.acquire(ctx, name)
	if err != nil {
		return err
	}
	release()
	return nil
}

// Stop stops a registered app.
func (d *Daemon) Stop(name string) error {
	d.mu.Lock()
	r := d.routes[name]
	if r == nil || r.app == nil {
		d.mu.Unlock()
		return ErrNotFound
	}
	p := d.stopLocked(r)
	d.mu.Unlock()
	d.stopProc(p)
	return nil
}

// Shutdown stops every registered app.
func (d *Daemon) Shutdown() {
	d.mu.Lock()
	var ps []*Process
	for _, r := range d.routes {
		if p := d.stopLocked(r); p != nil {
			ps = append(ps, p)
		}
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Go(func() { d.stopProc(p) })
	}
	wg.Wait()
}

// acquire returns the upstream port of name. It starts a stopped app and
// waits for it. The caller must call release when the request ends.
func (d *Daemon) acquire(ctx context.Context, name string) (port int, release func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.StartTimeout)
	defer cancel()
	waited := false
	for {
		var wait chan struct{}
		d.mu.Lock()
		r := d.routes[name]
		switch {
		case r == nil:
			d.mu.Unlock()
			return 0, nil, ErrNotFound
		case r.oneoff != nil:
			if r.oneoff.port != 0 {
				port := r.oneoff.port
				d.mu.Unlock()
				return port, func() {}, nil
			}
			wait = r.oneoff.ready
		case r.app == nil:
			d.mu.Unlock()
			return 0, nil, ErrNotFound
		case len(r.app.Cmd) == 0:
			port := r.app.Port
			d.mu.Unlock()
			return port, func() {}, nil
		case r.state == running:
			r.active++
			if r.idleStop != nil {
				r.idleStop.Stop()
			}
			port := r.port
			d.mu.Unlock()
			return port, func() { d.release(r) }, nil
		case r.state == crashed && waited:
			err := r.err
			d.mu.Unlock()
			return 0, nil, err
		case r.state == starting:
			wait = r.ready
		default:
			d.startLocked(r)
			wait = r.ready
		}
		d.mu.Unlock()

		select {
		case <-wait:
			waited = true
		case <-ctx.Done():
			return 0, nil, fmt.Errorf("%s did not start: %w", name, ctx.Err())
		}
	}
}

func (d *Daemon) release(r *route) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.active--
	d.armIdleLocked(r)
}

func (d *Daemon) armIdleLocked(r *route) {
	if r.active > 0 || r.state != running || r.app == nil || r.app.Idle <= 0 {
		return
	}
	gen := r.gen
	if r.idleStop != nil {
		r.idleStop.Stop()
	}
	r.idleStop = time.AfterFunc(time.Duration(r.app.Idle), func() {
		d.mu.Lock()
		var p *Process
		if r.gen == gen && r.active == 0 && r.state == running {
			p = d.stopLocked(r)
		}
		d.mu.Unlock()
		d.stopProc(p)
	})
}

func (d *Daemon) startLocked(r *route) {
	r.gen++
	r.state = starting
	r.err = nil
	r.ready = make(chan struct{})
	go d.run(r, r.gen, *r.app, r.ready)
}

// run starts the process of app and waits for its port.
func (d *Daemon) run(r *route, gen int, app App, ready chan struct{}) {
	p, port, err := d.spawn(app)

	d.mu.Lock()
	if r.gen != gen {
		d.mu.Unlock()
		close(ready)
		d.stopProc(p)
		return
	}
	if err != nil {
		r.state, r.err = crashed, err
		d.mu.Unlock()
		close(ready)
		d.stopProc(p)
		return
	}
	r.state, r.port, r.proc = running, port, p
	d.armIdleLocked(r)
	d.mu.Unlock()
	close(ready)

	<-p.Done()
	d.mu.Lock()
	if r.gen == gen {
		r.gen++
		r.state, r.port, r.proc = crashed, 0, nil
		r.err = fmt.Errorf("%s exited: %v\n\n%s", app.Name, p.Err(), tail(LogFile(d.cfg.Home, app.Name)))
	}
	d.mu.Unlock()
}

func (d *Daemon) spawn(app App) (*Process, int, error) {
	free, err := FreePort()
	if err != nil {
		return nil, 0, err
	}
	logPath := LogFile(d.cfg.Home, app.Name)
	log, err := os.Create(logPath)
	if err != nil {
		return nil, 0, err
	}
	defer log.Close() // the child keeps its own copy
	p, err := StartProcess(ProcessSpec{
		Dir: app.Dir, Cmd: app.Cmd, Env: app.Env, Port: free,
		Stdout: log, Stderr: log, NewGroup: true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("%s did not start: %w", app.Name, err)
	}

	known := app.Port
	if _, ok := Expand(app.Cmd, free); ok && known == 0 {
		known = free
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.StartTimeout)
	defer cancel()
	port, err := WaitPort(ctx, p, known)
	if err != nil {
		return p, 0, fmt.Errorf("%s did not start: %v\n\n%s", app.Name, err, tail(logPath))
	}
	return p, port, nil
}

// stopLocked marks r stopped and returns the process to stop. Call
// stopProc on the result after you unlock.
func (d *Daemon) stopLocked(r *route) *Process {
	r.gen++
	if r.idleStop != nil {
		r.idleStop.Stop()
	}
	p := r.proc
	r.proc, r.port, r.err = nil, 0, nil
	r.state = stopped
	return p
}

func (d *Daemon) stopProc(p *Process) {
	if p != nil {
		p.Stop(d.cfg.StopGrace)
	}
}

// Attachment is the route of a one-off run.
type Attachment struct {
	d *Daemon
	r *route
	o *oneoff
}

// Attach routes name to a one-off run. A registered app with the same name
// stops until the attachment closes. Requests wait until SetPort.
func (d *Daemon) Attach(name string) (*Attachment, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	r := d.routes[name]
	if r == nil {
		r = &route{name: name, state: stopped}
		d.routes[name] = r
	}
	if r.oneoff != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("another doze run uses %s", name)
	}
	p := d.stopLocked(r)
	o := &oneoff{ready: make(chan struct{})}
	r.oneoff = o
	d.mu.Unlock()
	d.stopProc(p)
	return &Attachment{d, r, o}, nil
}

// SetPort sets the upstream port and releases the waiting requests.
func (a *Attachment) SetPort(port int) {
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	if a.r.oneoff == a.o && a.o.port == 0 {
		a.o.port = port
		close(a.o.ready)
	}
}

// Close removes the route of the one-off run.
func (a *Attachment) Close() {
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	if a.r.oneoff != a.o {
		return
	}
	a.r.oneoff = nil
	if a.o.port == 0 {
		close(a.o.ready)
	}
	if a.r.app == nil && a.d.routes[a.r.name] == a.r {
		delete(a.d.routes, a.r.name)
	}
}

// tail returns the last 40 lines of a file.
func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}
