package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func serve(t *testing.T) (*Daemon, *Client) {
	t.Helper()
	home := shortTempDir(t)
	d, err := NewDaemon(Config{Home: home, ProxyPort: 8123})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	sock := filepath.Join(home, "doze.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go d.ServeControl(l)
	return d, NewClient(sock)
}

func TestRegisterListUnregister(t *testing.T) {
	_, c := serve(t)
	app := App{Name: "api", Dir: "/tmp", Cmd: []string{"go", "run", "."}, Idle: Duration(time.Minute)}
	if err := c.Register(app); err != nil {
		t.Fatal(err)
	}
	apps, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "api" || apps[0].State != "stopped" {
		t.Fatalf("List = %+v", apps)
	}
	if err := c.Unregister("api"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unregister("api"); err == nil {
		t.Fatal("second Unregister returned no error")
	}
}

func TestInfoReturnsProxyPort(t *testing.T) {
	_, c := serve(t)
	port, _, err := c.Ports()
	if err != nil || port != 8123 {
		t.Fatalf("ProxyPort = %d, %v", port, err)
	}
}

func TestAttachLastsUntilClose(t *testing.T) {
	d, c := serve(t)
	a, err := c.Attach("web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPort(5173); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		l := d.List()
		return len(l) == 1 && l[0].OneOff && l[0].Port == 5173
	})

	if _, err := c.Attach("web", ""); err == nil {
		t.Fatal("second Attach returned no error")
	}

	a.Close()
	waitFor(t, func() bool { return len(d.List()) == 0 })
}

func TestShutdownClosesQuit(t *testing.T) {
	d, c := serve(t)
	if err := c.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Quit():
	case <-time.After(time.Second):
		t.Fatal("Quit is not closed after Shutdown")
	}
}

func TestDialFailsWithoutDaemon(t *testing.T) {
	c := NewClient(filepath.Join(shortTempDir(t), "none.sock"))
	if _, err := c.List(); err == nil {
		t.Fatal("List returned no error without a daemon")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPingFailsWhenDaemonHangs(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "hang.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()

	done := make(chan bool)
	go func() { done <- NewClient(sock).Ping() }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Ping returned true for a daemon that does not answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ping did not return")
	}
}
