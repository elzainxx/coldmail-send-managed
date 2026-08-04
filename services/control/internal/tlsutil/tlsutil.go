// Package tlsutil builds the mTLS configuration for the worker<->Control channel. mTLS is
// required in both directions (CLAUDE.md Rule 6): the Control presents a server cert AND
// requires+verifies a client cert signed by the dev CA. There is no non-mTLS listener.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerConfig names the Control's own cert/key and the CA it trusts for client certs.
type ServerConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// Build returns a *tls.Config that REQUIRES and VERIFIES a client certificate. A peer that
// presents no cert, or one not signed by the CA, is rejected at the TLS layer.
func (c ServerConfig) Build() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file %q: no certs parsed", c.CAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert, // mTLS: client cert mandatory
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
