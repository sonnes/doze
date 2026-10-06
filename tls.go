// The CA signs a certificate for each <name>.localhost host, and for each
// host under a domain that doze setup --domain added, so that the proxy can
// serve HTTPS. doze setup adds the CA to the login keychain.

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 365 * 24 * time.Hour
	clockSkew    = time.Hour
	serialBits   = 128
)

// CA is the local certificate authority of doze.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// CAFile returns the path of the CA certificate in home.
func CAFile(home string) string { return filepath.Join(home, "ca.pem") }

func caKeyFile(home string) string { return filepath.Join(home, "ca-key.pem") }

// LoadCA reads the CA from home. It makes a new CA if home has none. The CA
// must stay the same, because doze setup trusts this file and not a key.
func LoadCA(home string) (*CA, error) {
	certPEM, certErr := os.ReadFile(CAFile(home))
	keyPEM, keyErr := os.ReadFile(caKeyFile(home))
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		return newCA(home, nil)
	}
	if certErr != nil {
		return nil, certErr
	}
	if keyErr != nil {
		return nil, keyErr
	}

	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("%s or %s is not PEM", CAFile(home), caKeyFile(home))
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{
		Cert:   cert,
		key:    key,
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// EnsureCA returns the CA in home. If the CA does not permit domain, EnsureCA
// replaces it with a CA that permits domain and every domain of the old CA.
// The name constraints of a CA cannot change, so a new domain needs a new CA,
// and doze setup must trust it again.
func EnsureCA(home, domain string) (*CA, error) {
	ca, err := LoadCA(home)
	if err != nil {
		return nil, err
	}
	if domain == "" || ca.Permits(domain) {
		return ca, nil
	}
	var domains []string
	for _, d := range ca.Cert.PermittedDNSDomains {
		if d != "localhost" {
			domains = append(domains, d)
		}
	}
	return newCA(home, append(domains, domain))
}

// newCA makes a CA that permits localhost and domains.
func newCA(home string, domains []string) (*CA, error) {
	permitted := append([]string{"localhost"}, domains...)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "doze local CA", Organization: []string{"doze"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// The CA is trusted by the browser. If its key leaks, the constraint
		// keeps it from signing a certificate for any other domain.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(caKeyFile(home), keyPEM, 0o600); err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(CAFile(home), certPEM, 0o644); err != nil {
		return nil, err
	}
	return &CA{
		Cert:   cert,
		key:    key,
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// Permits reports whether the name constraints of the CA include host.
func (ca *CA) Permits(host string) bool {
	host = strings.ToLower(host)
	for _, d := range ca.Cert.PermittedDNSDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// TLSConfig returns a server config that signs a certificate for each host
// on its first handshake. A wildcard for *.localhost does not work, because
// macOS rejects a wildcard directly under a top-level domain.
func (ca *CA) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: ca.certificate,
	}
}

func (ca *CA) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := strings.ToLower(hello.ServerName)
	if host == "" {
		host = "localhost"
	}
	if !ca.Permits(host) {
		return nil, fmt.Errorf("the doze CA does not permit %q. Run doze setup --domain", host)
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leaves[host]; ok {
		return c, nil
	}
	c, err := ca.sign(host)
	if err != nil {
		return nil, err
	}
	ca.leaves[host] = c
	return c, nil
}

func (ca *CA) sign(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
}
