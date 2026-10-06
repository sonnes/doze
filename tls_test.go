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

func TestCAWithDomainSignsItsNames(t *testing.T) {
	home := shortTempDir(t)
	ca, err := EnsureCA(home, "dev.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Permits("api.dev.example.com") || !ca.Permits("api.localhost") {
		t.Fatal("the CA does not permit the domain and localhost")
	}
	if ca.Permits("example.com") || ca.Permits("api.example.com") {
		t.Fatal("the CA permits a name outside the domain")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = ca.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	res, err := httpsClient(ca, "api.dev.example.com").Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if _, err := httpsClient(ca, "api.example.com").Get(srv.URL); err == nil {
		t.Error("handshake for api.example.com succeeded, want an error")
	}
}

func TestEnsureCAReplacesCAOnlyWhenTheDomainIsNew(t *testing.T) {
	home := shortTempDir(t)
	old, err := LoadCA(home)
	if err != nil {
		t.Fatal(err)
	}
	same, err := EnsureCA(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old.Cert.Raw, same.Cert.Raw) {
		t.Fatal("EnsureCA without a domain made a new CA")
	}

	withDomain, err := EnsureCA(home, "dev.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old.Cert.Raw, withDomain.Cert.Raw) {
		t.Fatal("EnsureCA kept a CA that does not permit the domain")
	}
	again, err := EnsureCA(home, "dev.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withDomain.Cert.Raw, again.Cert.Raw) {
		t.Fatal("EnsureCA replaced a CA that permits the domain")
	}
	second, err := EnsureCA(home, "dev.other.com")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Permits("api.dev.other.com") || !second.Permits("api.dev.example.com") || !second.Permits("api.localhost") {
		t.Fatal("a second domain dropped a domain that the CA permitted")
	}
	withDomain = second

	loaded, err := LoadCA(home)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withDomain.Cert.Raw, loaded.Cert.Raw) {
		t.Fatal("LoadCA does not read the new CA")
	}
}
