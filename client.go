// The client talks to the doze daemon over its control socket.

package main

import (
	"encoding/json"
	"errors"
	"net"
	"time"
)

// pingTimeout limits the wait for a ping. Without it, a daemon that does not
// answer stops every command.
const pingTimeout = 2 * time.Second

// Client sends requests to the daemon at a socket path.
type Client struct{ sock string }

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client { return &Client{path} }

func (c *Client) do(req Request) (Response, error) {
	return c.doTimeout(req, 0)
}

// doTimeout is do with a limit on the whole exchange. A timeout of 0 means
// no limit.
func (c *Client) doTimeout(req Request, timeout time.Duration) (Response, error) {
	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if timeout > 0 {
		conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return res, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// Ping reports whether the daemon answers.
func (c *Client) Ping() bool {
	_, err := c.doTimeout(Request{Op: "info"}, pingTimeout)
	return err == nil
}

// Ports returns the HTTP and HTTPS ports of the proxy. The HTTPS port is 0
// when the daemon does not serve HTTPS.
func (c *Client) Ports() (http, https int, err error) {
	res, err := c.do(Request{Op: "info"})
	return res.ProxyPort, res.HTTPSPort, err
}

// List returns the status of each app.
func (c *Client) List() ([]Status, error) {
	res, err := c.do(Request{Op: "list"})
	return res.Apps, err
}

// Register adds or replaces an app.
func (c *Client) Register(a App) error {
	_, err := c.do(Request{Op: "register", App: &a})
	return err
}

// Unregister stops and removes an app.
func (c *Client) Unregister(name string) error {
	_, err := c.do(Request{Op: "unregister", Name: name})
	return err
}

// Start starts an app and waits until it serves.
func (c *Client) Start(name string) error {
	_, err := c.do(Request{Op: "start", Name: name})
	return err
}

// Stop stops an app.
func (c *Client) Stop(name string) error {
	_, err := c.do(Request{Op: "stop", Name: name})
	return err
}

// Shutdown asks the daemon to stop its apps and exit.
func (c *Client) Shutdown() error {
	_, err := c.do(Request{Op: "shutdown"})
	return err
}

// ClientAttachment is the route of a one-off run. The route lasts until Close.
type ClientAttachment struct {
	conn      net.Conn
	enc       *json.Encoder
	ProxyPort int
	HTTPSPort int
}

// Attach routes name, and name.domain if domain is not empty, to a one-off
// run.
func (c *Client) Attach(name, domain string) (*ClientAttachment, error) {
	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(Request{Op: "attach", Name: name, Domain: domain}); err != nil {
		conn.Close()
		return nil, err
	}
	var res Response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		conn.Close()
		return nil, err
	}
	if res.Error != "" {
		conn.Close()
		return nil, errors.New(res.Error)
	}
	return &ClientAttachment{
		conn:      conn,
		enc:       enc,
		ProxyPort: res.ProxyPort,
		HTTPSPort: res.HTTPSPort,
	}, nil
}

// SetPort tells the daemon the port of the one-off run.
func (a *ClientAttachment) SetPort(port int) error {
	return a.enc.Encode(Request{Op: "port", Port: port})
}

// Close removes the route.
func (a *ClientAttachment) Close() error { return a.conn.Close() }
