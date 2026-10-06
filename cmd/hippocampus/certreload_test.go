package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

// The serving certificate reloads from disk (TODO-3 item 169). A certificate loaded once at startup
// meant every cert-manager rotation needed a restart, and a missed restart was an outage on the day
// the old certificate expired.

func servedLeaf(t *testing.T, r *certReloader) *x509.Certificate {
	t.Helper()

	cert, err := r.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %s", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %s", err)
	}

	return leaf
}

// rotate copies another pair's files over the reloader's, as a cert-manager rotation does, and moves
// their mtime forward so a coarse filesystem clock cannot hide the change.
func rotate(t *testing.T, certPath string, keyPath string, fromCert string, fromKey string, at time.Time) {
	t.Helper()

	for src, dst := range map[string]string{fromCert: certPath, fromKey: keyPath} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %s", src, err)
		}

		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatalf("write %s: %s", dst, err)
		}

		if err := os.Chtimes(dst, at, at); err != nil {
			t.Fatalf("chtimes: %s", err)
		}
	}
}

func TestCertReloaderServesARotatedCertificate(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)
	nextCert, nextKey := writeSelfSignedCert(t)

	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %s", err)
	}

	before := servedLeaf(t, r)

	rotate(t, certPath, keyPath, nextCert, nextKey, time.Now().Add(time.Minute))
	r.reloadIfChanged()

	after := servedLeaf(t, r)

	if bytes.Equal(before.Raw, after.Raw) {
		t.Fatal("the rotated certificate was not picked up")
	}

	if !r.notAfter().Equal(after.NotAfter) {
		t.Errorf("the reported expiry %s is not the served certificate's %s", r.notAfter(), after.NotAfter)
	}
}

// TestCertReloaderKeepsTheLastGoodPair: a rotation caught half-written - the certificate updated and
// the key not yet - must not take the listener's certificate away.
func TestCertReloaderKeepsTheLastGoodPair(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)

	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %s", err)
	}

	before := servedLeaf(t, r)

	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %s", err)
	}

	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(certPath, later, later)

	r.reloadIfChanged()

	if after := servedLeaf(t, r); !bytes.Equal(before.Raw, after.Raw) {
		t.Error("a broken pair replaced the working certificate")
	}
}

func TestCertReloaderRefusesABadInitialPair(t *testing.T) {
	if _, err := newCertReloader("/nonexistent/cert.pem", "/nonexistent/key.pem"); err == nil {
		t.Error("an unreadable pair was accepted at startup")
	}
}
