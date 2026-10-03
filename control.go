package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
)

// Request is one line of JSON that a client sends on the control socket.
type Request struct {
	Op   string `json:"op"` // info, list, register, unregister, start, stop, attach, port, shutdown
	Name string `json:"name,omitempty"`
	App  *App   `json:"app,omitempty"`
	Port int    `json:"port,omitempty"`
}

// Response is one line of JSON that the daemon sends back.
type Response struct {
	Error     string   `json:"error,omitempty"`
	Apps      []Status `json:"apps,omitempty"`
	ProxyPort int      `json:"proxy_port,omitempty"`
	HTTPSPort int      `json:"https_port,omitempty"`
}

// Quit is closed when a client asks the daemon to shut down.
func (d *Daemon) Quit() <-chan struct{} { return d.quit }

// ServeControl serves the control protocol on l until l closes. Each
// connection carries one request and one response. An attach connection
// stays open: the client sends port requests on it, and the route goes away
// when the connection closes.
func (d *Daemon) ServeControl(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go d.serveConn(c)
	}
}

func (d *Daemon) serveConn(c net.Conn) {
	defer c.Close()
	dec := json.NewDecoder(bufio.NewReader(c))
	enc := json.NewEncoder(c)
	var req Request
	if err := dec.Decode(&req); err != nil {
		return
	}

	if req.Op == "attach" {
		a, err := d.Attach(req.Name)
		if err != nil {
			enc.Encode(Response{Error: err.Error()})
			return
		}
		defer a.Close()
		enc.Encode(Response{ProxyPort: d.cfg.ProxyPort, HTTPSPort: d.cfg.HTTPSPort})
		for dec.Decode(&req) == nil {
			if req.Op == "port" {
				a.SetPort(req.Port)
			}
		}
		return
	}

	var res Response
	var err error
	switch req.Op {
	case "info":
		res.ProxyPort = d.cfg.ProxyPort
		res.HTTPSPort = d.cfg.HTTPSPort
	case "list":
		res.Apps = d.List()
	case "register":
		if req.App == nil {
			res.Error = "no app"
			break
		}
		err = d.Register(*req.App)
	case "unregister":
		err = d.Unregister(req.Name)
	case "start":
		err = d.Start(context.Background(), req.Name)
	case "stop":
		err = d.Stop(req.Name)
	case "shutdown":
		d.quitOnce.Do(func() { close(d.quit) })
	default:
		res.Error = "unknown op " + req.Op
	}
	if err != nil {
		res.Error = err.Error()
	}
	enc.Encode(res)
}
