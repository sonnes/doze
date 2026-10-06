package main

import (
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// Handler returns the proxy. It routes <name>.localhost, and
// <name>.<domain> for an app with a domain, to the app name.
func (d *Daemon) Handler() http.Handler {
	return http.HandlerFunc(d.serveProxy)
}

func (d *Daemon) serveProxy(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	name, ok := strings.CutSuffix(host, ".localhost")
	if !ok {
		name, ok = d.nameOfDomainHost(host)
	}
	if !ok {
		d.serveOther(w, r, host)
		return
	}

	port, release, err := d.acquire(r.Context(), name)
	if errors.Is(err, ErrNotFound) {
		d.serveIndex(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer release()

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("%s on port %d: %v", name, port, err), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// nameOfDomainHost returns the name of the route whose <name>.<domain> is
// host.
func (d *Daemon) nameOfDomainHost(host string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.routes {
		if dom := r.domain(); dom != "" && host == r.name+"."+dom {
			return r.name, true
		}
	}
	return "", false
}

// serveOther serves a host that is not <name>.localhost. It lists the apps
// only for a loopback host. A DNS rebinding page sends its own host, and it
// must not read the list.
func (d *Daemon) serveOther(w http.ResponseWriter, r *http.Request, host string) {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		http.Error(w, "doze serves only localhost names", http.StatusNotFound)
		return
	}
	d.serveIndex(w, r, http.StatusOK)
}

// serveIndex lists the apps with links.
func (d *Daemon) serveIndex(w http.ResponseWriter, r *http.Request, code int) {
	_, port, _ := net.SplitHostPort(r.Host)
	scheme := "http://"
	if r.TLS != nil {
		scheme = "https://"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>doze</title>`+
		`<style>body{font:15px/1.6 ui-monospace,Menlo,monospace;margin:40px}td{padding:2px 16px 2px 0}</style>`)
	if code == http.StatusNotFound {
		fmt.Fprintf(w, "<p>No app for %s.</p>", html.EscapeString(r.Host))
	}
	fmt.Fprint(w, "<table>")
	for _, s := range d.List() {
		u := scheme + s.Name + ".localhost"
		if port != "" {
			u += ":" + port
		}
		fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td>%s</td></tr>`,
			html.EscapeString(u), html.EscapeString(strings.TrimPrefix(u, scheme)), s.State)
	}
	fmt.Fprint(w, "</table>")
}

// LoopbackOnly returns a listener that closes connections from other hosts.
func LoopbackOnly(l net.Listener) net.Listener { return loopback{l} }

type loopback struct{ net.Listener }

func (l loopback) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
			return c, nil
		}
		c.Close()
	}
}
