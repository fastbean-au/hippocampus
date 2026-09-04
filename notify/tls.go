package notify

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	log "github.com/sirupsen/logrus"
)

// TLSConfig carries the optional TLS settings for the callback connection. Every field is
// empty/false by default, in which case the client relies on the address scheme alone (an https://
// URL verifies against the system certificate pool with no customisation).
//
// The same four options as opensearch.tls and transfer.tls, deliberately: an operator who has
// already configured a private CA for one of those should not have to learn a second vocabulary
// for this one.
type TLSConfig struct {
	// CACertFile is a PEM bundle of certificate authorities to trust for the server certificate,
	// used in place of the system pool. Set it to trust a receiver serving a certificate signed by
	// a private CA.
	CACertFile string

	// CertFile and KeyFile are a client certificate/key pair for mutual TLS. Both must be set
	// together, or neither.
	CertFile string
	KeyFile  string

	// InsecureSkipVerify disables server certificate verification entirely. It is a
	// development-only escape hatch for self-signed certificates - prefer CACertFile in
	// production, where an unverified connection offers no protection against interception.
	InsecureSkipVerify bool
}

// build assembles the *tls.Config from the TLS settings, or returns a nil config when no TLS
// customisation is requested (the client's default behaviour then applies unchanged). It fails on
// an unreadable or empty CA bundle, a half-configured client certificate pair, or an unloadable
// key pair.
func (c TLSConfig) build() (*tls.Config, error) {
	if c.CACertFile == "" && c.CertFile == "" && c.KeyFile == "" && !c.InsecureSkipVerify {
		return nil, nil
	}

	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, fmt.Errorf("callbacks tls client certificate requires both certFile and keyFile, or neither")
	}

	out := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.InsecureSkipVerify,
	}

	if c.CACertFile != "" {
		pem, err := os.ReadFile(c.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("reading callbacks CA cert file %q: %w", c.CACertFile, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("callbacks CA cert file %q contained no valid certificates", c.CACertFile)
		}

		out.RootCAs = pool
	}

	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading callbacks client certificate: %w", err)
		}

		out.Certificates = []tls.Certificate{cert}
	}

	return out, nil
}

// buildTransport chooses the HTTP transport for the callback client. A test-supplied cfg.Transport
// wins outright. Otherwise, when the TLS block requests any customisation, it clones the default
// transport - keeping its pooling, timeouts, and proxy behaviour - and installs the configured
// *tls.Config. With no TLS customisation it returns nil so the client keeps the default transport.
func buildTransport(cfg Config) (http.RoundTripper, error) {
	if cfg.Transport != nil {
		return cfg.Transport, nil
	}

	tlsConfig, err := cfg.TLS.build()
	if err != nil {
		return nil, err
	}

	if tlsConfig == nil {
		return nil, nil
	}

	if cfg.TLS.InsecureSkipVerify {
		log.Warn("callbacks tls certificate verification is disabled (insecureSkipVerify) - do not use in production")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig

	return transport, nil
}
