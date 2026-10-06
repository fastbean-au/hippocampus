package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// certReloadInterval is how often the serving certificate's files are checked for a change. A
// rotation is noticed within it, and a stat of two files a minute costs nothing.
const certReloadInterval = time.Minute

// certReloader serves the TLS certificate from disk and reloads it when its files change (TODO-3
// item 169). A pair loaded once at startup meant every cert-manager rotation needed a restart, and a
// restart that did not happen was an outage on the day the old certificate expired.
//
// It follows auth.RevocationList's shape: a bad pair at startup fails it, and a bad pair later -
// commonly a rotation caught between writing the certificate and writing the key - is logged and the
// last good pair keeps serving. Both listeners share the one tls.Config, so a reload reaches both.
type certReloader struct {
	certFile string
	keyFile  string

	current atomic.Pointer[tls.Certificate]
	expiry  atomic.Int64

	mu         sync.Mutex
	certStamp  time.Time
	keyStamp   time.Time
	stop       chan struct{}
	stopOnce   sync.Once
	expiryHint metric.Int64Gauge
}

func newCertReloader(certFile string, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, stop: make(chan struct{})}

	gauge, err := otel.Meter(interceptorScopeName).Int64Gauge(
		"hippocampus.tls.certificate_not_after",
		metric.WithDescription("Unix seconds at which the serving TLS certificate expires, published only while tls.enabled; it moves when a rotated certificate is picked up."),
	)
	if err != nil {
		log.Errorf("failed to create the certificate expiry gauge: %s", err.Error())
	}

	r.expiryHint = gauge

	if err := r.load(); err != nil {
		return nil, err
	}

	return r, nil
}

// GetCertificate is the tls.Config hook both listeners call per handshake.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

// notAfter is when the certificate currently served expires.
func (r *certReloader) notAfter() time.Time {
	return time.Unix(r.expiry.Load(), 0)
}

// start polls for a change until stop.
func (r *certReloader) start() {
	go func() {
		ticker := time.NewTicker(certReloadInterval)
		defer ticker.Stop()

		for {
			select {

			case <-r.stop:
				return

			case <-ticker.C:
				r.reloadIfChanged()

			}
		}
	}()
}

func (r *certReloader) close() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// reloadIfChanged reloads when either file's mtime has moved, keeping the last good pair on failure.
func (r *certReloader) reloadIfChanged() {
	certStamp, keyStamp, err := r.stamps()
	if err != nil {
		log.Warnf("checking the TLS certificate for a rotation: %s; the current certificate keeps serving", err.Error())

		return
	}

	r.mu.Lock()
	unchanged := certStamp.Equal(r.certStamp) && keyStamp.Equal(r.keyStamp)
	r.mu.Unlock()

	if unchanged {
		return
	}

	if err := r.load(); err != nil {
		log.Warnf("reloading the rotated TLS certificate: %s; the previous certificate keeps serving until the pair is readable", err.Error())

		return
	}

	log.WithFields(log.Fields{
		"certificate": r.certFile,
		"not_after":   r.notAfter().UTC().Format(time.RFC3339),
	}).
		Info("reloaded the rotated TLS certificate")
}

func (r *certReloader) stamps() (time.Time, time.Time, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	return certInfo.ModTime(), keyInfo.ModTime(), nil
}

func (r *certReloader) load() error {
	certStamp, keyStamp, err := r.stamps()
	if err != nil {
		return err
	}

	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parsing the certificate: %w", err)
	}

	pair.Leaf = leaf

	r.current.Store(&pair)
	r.expiry.Store(leaf.NotAfter.Unix())

	if r.expiryHint != nil {
		r.expiryHint.Record(context.Background(), leaf.NotAfter.Unix())
	}

	r.mu.Lock()
	r.certStamp, r.keyStamp = certStamp, keyStamp
	r.mu.Unlock()

	return nil
}
