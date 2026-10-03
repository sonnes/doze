package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newDaemon(t *testing.T, home string) (*Daemon, *httptest.Server) {
	t.Helper()
	d, err := NewDaemon(Config{Home: home, StartTimeout: 10 * time.Second, StopGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return d, srv
}

func get(t *testing.T, srv *httptest.Server, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/hello", nil)
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func status(t *testing.T, d *Daemon, name string) Status {
	t.Helper()
	for _, s := range d.List() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no app %q", name)
	return Status{}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func helperApp(t *testing.T, name string, args ...string) App {
	return App{
		Name: name,
		Dir:  t.TempDir(),
		Cmd:  append([]string{helperPath(t)}, args...),
		Idle: Duration(time.Minute),
	}
}

func TestLazyStartStartsOnceForConcurrentRequests(t *testing.T) {
	home := shortTempDir(t)
	d, srv := newDaemon(t, home)
	count := filepath.Join(home, "count")
	if err := d.Register(helperApp(t, "api", "-count", count)); err != nil {
		t.Fatal(err)
	}
	if s := status(t, d, "api"); s.State != "stopped" {
		t.Fatalf("state after register = %q, want stopped", s.State)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			code, body := get(t, srv, "api.localhost")
			if code != 200 || !strings.Contains(body, "host=api.localhost") {
				t.Errorf("got %d %q", code, body)
			}
		})
	}
	wg.Wait()

	data, _ := os.ReadFile(count)
	if n := strings.Count(string(data), "start"); n != 1 {
		t.Fatalf("app started %d times, want 1", n)
	}
	if s := status(t, d, "api"); s.State != "running" || s.Port == 0 {
		t.Fatalf("status = %+v, want running with a port", s)
	}
}

func TestProxyForwardsHostHeaders(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "web"))

	_, body := get(t, srv, "web.localhost:8080")
	if !strings.Contains(body, "host=web.localhost:8080") || !strings.Contains(body, "forwarded=web.localhost:8080") {
		t.Fatalf("body = %q", body)
	}
}

func TestPortPlaceholderGivesTheAppItsPort(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "api", "-addr", "127.0.0.1:{port}"))

	if code, body := get(t, srv, "api.localhost"); code != 200 {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestIdleAppStops(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	app := helperApp(t, "api")
	app.Idle = Duration(200 * time.Millisecond)
	d.Register(app)

	get(t, srv, "api.localhost")
	port := status(t, d, "api").Port
	eventually(t, "app stops when idle", func() bool { return status(t, d, "api").State == "stopped" })
	if Dial(port) {
		t.Fatal("app still listens after idle stop")
	}

	if code, _ := get(t, srv, "api.localhost"); code != 200 {
		t.Fatalf("request after idle stop got %d, want 200", code)
	}
}

func TestOpenUpgradedConnectionKeepsAppRunning(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	app := helperApp(t, "api")
	app.Idle = Duration(200 * time.Millisecond)
	d.Register(app)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: api.localhost\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n"))
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	if err != nil || res.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", res, err)
	}

	time.Sleep(600 * time.Millisecond)
	if s := status(t, d, "api"); s.State != "running" {
		t.Fatalf("state with open connection = %q, want running", s.State)
	}
	conn.Write([]byte("ping\n"))
	if line, _ := r.ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo = %q", line)
	}

	conn.Close()
	eventually(t, "app stops after the connection closes", func() bool { return status(t, d, "api").State == "stopped" })
}

func TestStartFailureShowsLogTail(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "api", "-exit", "1"))

	code, body := get(t, srv, "api.localhost")
	if code != http.StatusBadGateway || !strings.Contains(body, "boom: helper exited") {
		t.Fatalf("got %d %q, want 502 with the log", code, body)
	}
	if s := status(t, d, "api"); s.State != "crashed" {
		t.Fatalf("state = %q, want crashed", s.State)
	}
}

func TestUnknownHostListsApps(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "api"))

	code, body := get(t, srv, "nope.localhost")
	if code != http.StatusNotFound || !strings.Contains(body, "api.localhost") {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestRegisteredAppsPersist(t *testing.T) {
	home := shortTempDir(t)
	d, _ := newDaemon(t, home)
	app := helperApp(t, "api")
	app.Env = []string{"PATH=/custom/bin"}
	d.Register(app)
	d.Shutdown()

	d2, _ := newDaemon(t, home)
	s := status(t, d2, "api")
	if s.State != "stopped" || s.Dir != app.Dir || s.Idle != app.Idle {
		t.Fatalf("status after reload = %+v", s)
	}

	d2.Unregister("api")
	d3, _ := newDaemon(t, home)
	if len(d3.List()) != 0 {
		t.Fatalf("apps after unregister = %+v", d3.List())
	}
}

func TestRegisterRejectsBadNames(t *testing.T) {
	d, _ := newDaemon(t, shortTempDir(t))
	for _, name := range []string{"", "Web", "a b", "-x", "web.localhost"} {
		app := helperApp(t, name)
		if err := d.Register(app); err == nil {
			t.Errorf("Register(%q) returned no error", name)
		}
	}
}

func TestAttachReplacesRegisteredApp(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "web"))
	get(t, srv, "web.localhost")
	registeredPort := status(t, d, "web").Port

	// A one-off server, like `doze pnpm dev`, outside the daemon.
	port, _ := FreePort()
	p, _ := StartProcess(ProcessSpec{Cmd: []string{helperPath(t), "-port-env"}, Port: port, NewGroup: true})
	t.Cleanup(func() { p.Stop(time.Second) })

	a, err := d.Attach("web")
	if err != nil {
		t.Fatal(err)
	}
	if Dial(registeredPort) {
		t.Fatal("registered app still runs after attach")
	}
	if _, err := d.Attach("web"); err == nil {
		t.Fatal("second attach to the same name returned no error")
	}

	// A request before the port is known waits for it.
	done := make(chan int)
	go func() { code, _ := get(t, srv, "web.localhost"); done <- code }()
	time.Sleep(100 * time.Millisecond)
	a.SetPort(port)
	if code := <-done; code != 200 {
		t.Fatalf("held request got %d", code)
	}
	if s := status(t, d, "web"); s.Port != port || !s.OneOff {
		t.Fatalf("status = %+v, want one-off on %d", s, port)
	}

	a.Close()
	if s := status(t, d, "web"); s.State != "stopped" || s.OneOff {
		t.Fatalf("status after close = %+v, want stopped registered app", s)
	}
}

func TestAttachWithoutRegistrationRemovesRouteOnClose(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	a, err := d.Attach("tmp")
	if err != nil {
		t.Fatal(err)
	}
	port, _ := FreePort()
	p, _ := StartProcess(ProcessSpec{Cmd: []string{helperPath(t), "-port-env"}, Port: port, NewGroup: true})
	t.Cleanup(func() { p.Stop(time.Second) })
	Dial(port)
	a.SetPort(port)
	eventually(t, "one-off serves", func() bool { code, _ := get(t, srv, "tmp.localhost"); return code == 200 })

	a.Close()
	if code, _ := get(t, srv, "tmp.localhost"); code != http.StatusNotFound {
		t.Fatalf("got %d after close, want 404", code)
	}
}

func TestStopAndStart(t *testing.T) {
	d, _ := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "api"))

	if err := d.Start(t.Context(), "api"); err != nil {
		t.Fatal(err)
	}
	port := status(t, d, "api").Port
	if !Dial(port) {
		t.Fatal("app does not listen after Start")
	}
	if err := d.Stop("api"); err != nil {
		t.Fatal(err)
	}
	if s := status(t, d, "api"); s.State != "stopped" || Dial(port) {
		t.Fatalf("status after Stop = %+v", s)
	}
}

func TestOtherHostDoesNotListApps(t *testing.T) {
	d, srv := newDaemon(t, shortTempDir(t))
	d.Register(helperApp(t, "api"))

	// A DNS rebinding page sends its own host name.
	code, body := get(t, srv, "evil.example")
	if code != http.StatusNotFound || strings.Contains(body, "api") {
		t.Fatalf("got %d %q, want 404 without the apps", code, body)
	}
	if code, body := get(t, srv, "localhost"); code != 200 || !strings.Contains(body, "api.localhost") {
		t.Fatalf("localhost got %d %q, want the apps", code, body)
	}
}
