// The CA signs a certificate for each <name>.localhost host, so that the
// proxy can serve HTTPS. doze setup adds the CA to the login keychain.

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
		return newCA(home)
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

func newCA(home string) (*CA, error) {
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
		// keeps it from signing a certificate for a real domain.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
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
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("doze serves only localhost names, not %q", host)
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
