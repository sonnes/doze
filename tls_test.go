package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func httpsClient(ca *CA, host string) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: host,
			},
			ForceAttemptHTTP2: true,
		},
	}
}

func TestHTTPSServesEachHostWithItsCertificate(t *testing.T) {
	home := shortTempDir(t)
	d, _ := newDaemon(t, home)
	ca, err := LoadCA(home)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(d.Handler())
	srv.TLS = ca.TLSConfig()
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	if err := d.Register(helperApp(t, "api.web")); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "api.web.localhost"
	res, err := httpsClient(ca, "api.web.localhost").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "host=api.web.localhost") {
		t.Fatalf("got %d %q", res.StatusCode, body)
	}
	if res.ProtoMajor != 2 {
		t.Errorf("protocol = %s, want HTTP/2", res.Proto)
	}

	if _, err := httpsClient(ca, "example.com").Get(srv.URL); err == nil {
		t.Error("handshake for example.com succeeded, want an error")
	}
}

func TestLoadCAKeepsTheSameCA(t *testing.T) {
	home := shortTempDir(t)
	a, err := LoadCA(home)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadCA(home)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Cert.Raw, b.Cert.Raw) {
		t.Error("the second LoadCA made a new CA")
	}
}

func TestCASignsOnlyLocalhostNames(t *testing.T) {
	ca, err := LoadCA(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	for host, valid := range map[string]bool{
		"localhost":         true,
		"api.web.localhost": true,
		"example.com":       false,
	} {
		leaf, err := ca.sign(host)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(leaf.Certificate[0])
		_, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: host})
		if valid && err != nil {
			t.Errorf("%s: %v", host, err)
		}
		if !valid && err == nil {
			t.Errorf("%s: a certificate outside localhost verified", host)
		}
	}
}
