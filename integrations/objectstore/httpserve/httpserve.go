// Package httpserve runs one HTTP listener until its context is cancelled, then drains it.
//
// It exists because both commands in this module serve an inbound HTTP surface carrying a bearer
// token - the gateway's object routes and the reaper's callback endpoint - and the shutdown and TLS
// handling should not be written twice and then diverge. What differs between them is passed in:
// the gateway sets no write timeout, because proxy mode streams objects and an object large enough
// to exceed any chosen figure is exactly what it is for.
package httpserve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

// Config describes one listener.
type Config struct {
	// Handler serves the requests. Required.
	Handler http.Handler

	// Name appears in log lines and error messages ("objects", "callbacks").
	Name string

	// BindAddress restricts the interface; empty binds all of them.
	BindAddress string

	// Port is the TCP port.
	Port int

	// TLSCertFile and TLSKeyFile enable HTTPS (both or neither).
	TLSCertFile string
	TLSKeyFile  string

	// WriteTimeout bounds writing a response. Zero leaves it unbounded, which a streaming handler
	// needs.
	WriteTimeout time.Duration

	// ShutdownTimeout bounds the drain. Non-positive selects defaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

const (
	defaultShutdownTimeout = 10 * time.Second
	readHeaderTimeout      = 5 * time.Second
	idleTimeout            = 60 * time.Second
)

// Serve listens until ctx is cancelled, returning ctx.Err() on a clean shutdown and the listener's
// own error otherwise.
func Serve(ctx context.Context, cfg Config) error {
	log.Trace("func() httpserve.Serve")

	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return fmt.Errorf("serving %s over TLS requires both a certificate and a key, or neither", cfg.Name)
	}

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port)),
		Handler:           cfg.Handler,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       idleTimeout,
	}

	errs := make(chan error, 1)

	go func() {
		errs <- listen(server, cfg)
	}()

	select {

	case err := <-errs:
		if err != nil {
			return fmt.Errorf("serving %s: %w", cfg.Name, err)
		}

		return nil

	case <-ctx.Done():
		timeout := cfg.ShutdownTimeout
		if timeout <= 0 {
			timeout = defaultShutdownTimeout
		}

		// The parent context is already cancelled, so the drain needs its own.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Errorf("draining the %s listener: %s", cfg.Name, err.Error())
		}

		return ctx.Err()

	}
}

func listen(server *http.Server, cfg Config) error {
	var err error

	if cfg.TLSCertFile != "" {
		err = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		err = server.ListenAndServe()
	}

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
