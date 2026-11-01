package sandbox

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// tlsBridge serves with a run's certificate, as the bridge does, and counts
// the requests that reached it.
func tlsBridge(t *testing.T) (Certificate, *httptest.Server, *atomic.Int32) {
	t.Helper()
	certificate, err := NewCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certificate.CertPEM, certificate.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return certificate, server, &reached
}

func connectionTo(t *testing.T, server *httptest.Server, fingerprint string) Connection {
	t.Helper()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return Connection{Host: host, Port: number, Token: "t", Fingerprint: fingerprint}
}

func TestTheCertificateMatchesItsFingerprint(t *testing.T) {
	certificate, err := NewCertificate()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificate.CertPEM)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if CertificateFingerprint(parsed.Raw) != certificate.Fingerprint || !ValidFingerprint(certificate.Fingerprint) {
		t.Fatalf("fingerprint %q", certificate.Fingerprint)
	}
	if other, _ := NewCertificate(); other.Fingerprint == certificate.Fingerprint {
		t.Fatal("two runs got the same certificate")
	}
}

func TestClientsReachOnlyTheBridgeWithThePinnedCertificate(t *testing.T) {
	certificate, server, reached := tlsBridge(t)

	right := connectionTo(t, server, certificate.Fingerprint)
	if url := right.BridgeURL("/status"); url[:8] != "https://" {
		t.Fatalf("url %s", url)
	}
	response, err := right.Client(5 * time.Second).Get(right.BridgeURL("/status"))
	if err != nil {
		t.Fatalf("the right certificate was refused: %v", err)
	}
	response.Body.Close()

	// Another certificate (someone standing in for the bridge): refused before
	// any request, and so the token, is sent.
	other, _ := NewCertificate()
	wrong := connectionTo(t, server, other.Fingerprint)
	if _, err := wrong.Client(5 * time.Second).Get(wrong.BridgeURL("/status")); err == nil {
		t.Fatal("a certificate with another fingerprint was accepted")
	}
	if reached.Load() != 1 {
		t.Fatalf("%d requests reached the server", reached.Load())
	}

	// Without a fingerprint (the page's plain HTTP): http.
	if url := connectionTo(t, server, "").BridgeURL("/status"); url[:7] != "http://" {
		t.Fatalf("url %s", url)
	}
}
