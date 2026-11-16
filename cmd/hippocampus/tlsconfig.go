package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// loadServerTLS builds the TLS configuration shared by the gRPC listener and the HTTP gateway from
// the configured certificate/key pair, pinning a TLS 1.2 minimum. Go's current default server
// minimum is already TLS 1.2, but pinning it makes the floor explicit and immune to a future
// default change, keeping weak legacy protocol versions off both listeners.
//
// clientCAFile turns on the other direction. Every outbound connection this project makes carries a
// certFile/keyFile pair for mutual TLS - opensearch.tls, transfer.tls, callbacks.tls, the MCP
// bridge, the hippo CLI and the event-source bridges, six implementations of one block - and until
// this existed no listener here ever asked for a client certificate, so all six could only be used
// against somebody else's terminator. Presenting a client certificate to a server that never
// requests one fails silently, in the sense that nothing fails: the handshake completes, the
// certificate is not sent, and the operator believes they have mutual TLS.
//
// It composes with the JWT auth rather than replacing it, and the two answer different questions: a
// certificate says which PROCESS is connecting, a token says which client it is acting as and at
// what tier. Nothing here derives an identity, a tier or a group scope from the certificate - the
// authorisation model has one source, and a second one that silently outranked it would be worse
// than none.
func loadServerTLS(certFile string, keyFile string, clientCAFile string, requireClientCert bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	conf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if clientCAFile == "" {
		// Refused rather than honoured, because Go verifies against the system roots when ClientCAs
		// is nil: requiring a client certificate with no CA of your own admits every certificate any
		// public CA has ever issued, which is not the restriction the key reads as.
		if requireClientCert {
			return nil, fmt.Errorf("tls.requireClientCert is set without tls.clientCaFile: a client certificate can only be required against a CA bundle you nominate")
		}

		return conf, nil
	}

	pem, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading tls.clientCaFile: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls.clientCaFile %q contains no PEM certificates", clientCAFile)
	}

	conf.ClientCAs = pool

	// The two modes are a genuine choice rather than a strictness dial. Requiring a certificate is
	// the one an operator usually wants, but it locks out every client that has not been issued one
	// - including a browser reaching the console - so the default verifies a certificate when one is
	// offered and admits a connection without one, which is what makes the key safe to turn on
	// before the clients have been rolled out.
	conf.ClientAuth = tls.VerifyClientCertIfGiven

	if requireClientCert {
		conf.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return conf, nil
}

// serverTLSConfig is what initServerTLS needs to build the listeners' TLS configuration.
type serverTLSConfig struct {
	Enabled           bool
	CertFile          string
	KeyFile           string
	ClientCAFile      string
	RequireClientCert bool

	// GatewayEnabled is read alongside, since a required client certificate applies to the
	// gateway's probes too.
	GatewayEnabled bool
}

// serverTLSConfigFromViper reads the tls.* keys.
func serverTLSConfigFromViper() serverTLSConfig {
	log.Trace("func() serverTLSConfigFromViper")

	return serverTLSConfig{
		Enabled:           viper.GetBool("tls.enabled"),
		CertFile:          viper.GetString("tls.certFile"),
		KeyFile:           viper.GetString("tls.keyFile"),
		ClientCAFile:      viper.GetString("tls.clientCaFile"),
		RequireClientCert: viper.GetBool("tls.requireClientCert"),
		GatewayEnabled:    viper.GetInt("gateway.port") > 0,
	}
}

// initServerTLS builds the TLS configuration both listeners share, or nil with TLS off. Built once
// and shared so the gRPC service and the HTTP gateway present the same certificate and enforce the
// same TLS floor, and built before either listener starts so a bad certificate/key pair fails fast.
//
// The returned stop function ends the certificate reloader; it is never nil.
func initServerTLS(cfg serverTLSConfig) (*tls.Config, func(), error) {
	log.Trace("func() initServerTLS")

	if !cfg.Enabled {
		return nil, func() {}, nil
	}

	tlsConf, err := loadServerTLS(cfg.CertFile, cfg.KeyFile, cfg.ClientCAFile, cfg.RequireClientCert)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load TLS credentials: %w", err)
	}

	// Served through GetCertificate rather than as a fixed pair, so a rotated certificate is
	// picked up without a restart; both listeners share this config (TODO-3 item 169).
	reloader, err := newCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load TLS credentials: %w", err)
	}

	tlsConf.Certificates = nil
	tlsConf.GetCertificate = reloader.GetCertificate

	reloader.start()

	if cfg.ClientCAFile != "" {
		mode := "verified when offered"

		if cfg.RequireClientCert {
			mode = "required"
		}

		log.Infof("mutual TLS enabled on both listeners: client certificates %s against %s", mode, cfg.ClientCAFile)

		// The probes are exempt from authentication but not from the handshake, which happens
		// before a request has a path: requiring a certificate makes /healthz and /readyz
		// unreachable to an orchestrator that does not carry one, and a failing liveness probe
		// restarts a perfectly healthy instance.
		if cfg.RequireClientCert && cfg.GatewayEnabled {
			log.Warn("tls.requireClientCert applies to the gateway's /healthz and /readyz too, since the handshake precedes the request: give the probe client a certificate, or terminate TLS ahead of this listener")
		}
	}

	return tlsConf, reloader.close, nil
}
