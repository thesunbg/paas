// Package pki is the cluster's internal CA: it mints the TLS certs used by
// the embedded NATS server (clients verify it via the CA) and by NATS routes
// between control planes (mutual TLS).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"time"
)

// ServerName is the SAN every minted server cert carries and the name
// clients verify against, so cert validity does not depend on node IPs.
const ServerName = "nats.paas.internal"

const (
	caTTL     = 10 * 365 * 24 * time.Hour
	leafTTL   = 365 * 24 * time.Hour
	renewLeft = 30 * 24 * time.Hour
)

// CA is a parsed cluster CA able to sign leaf certs.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
	now  func() time.Time // injectable for tests
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA creates a self-signed ECDSA P-256 CA and returns it PEM-encoded.
func NewCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "paas cluster CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caTTL),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

// ParseCA loads a CA from the PEMs produced by NewCA.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("pki: invalid CA PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("pki: certificate is not a CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert: cert, key: key, pool: pool, now: time.Now}, nil
}

// Pool returns a pool containing only this CA, for RootCAs/ClientCAs.
func (c *CA) Pool() *x509.CertPool { return c.pool }

// Issue mints a leaf cert for names (DNS SANs) valid for ttl. It always has
// server-auth EKU, plus client-auth when client is true.
func (c *CA) Issue(names []string, ttl time.Duration, client bool) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	sn, err := serial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := c.now()
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if client {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// renewing caches a ServerName cert and re-mints it when <30 days remain.
type renewing struct {
	ca     *CA
	client bool
	mu     sync.Mutex
	cert   *tls.Certificate
}

func (r *renewing) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert == nil || r.cert.Leaf.NotAfter.Sub(r.ca.now()) < renewLeft {
		c, err := r.ca.Issue([]string{ServerName}, leafTTL, r.client)
		if err != nil {
			return nil, err
		}
		r.cert = &c
	}
	return r.cert, nil
}

// ServerTLS is for the NATS client listener; clients verify it with Pool().
func (c *CA) ServerTLS() *tls.Config {
	r := &renewing{ca: c}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.get() },
	}
}

// RouteTLS works as both ends of a route connection: it presents a
// server+client cert and requires/verifies the peer's cert against the CA.
func (c *CA) RouteTLS() *tls.Config {
	r := &renewing{ca: c, client: true}
	return &tls.Config{
		MinVersion:           tls.VersionTLS12,
		ClientAuth:           tls.RequireAndVerifyClientCert,
		ClientCAs:            c.pool,
		RootCAs:              c.pool,
		ServerName:           ServerName,
		GetCertificate:       func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.get() },
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return r.get() },
	}
}
