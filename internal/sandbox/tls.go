package sandbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"time"
)

// The bridge serves TLS with a certificate the app makes for each run. No
// authority vouches for it: its fingerprint travels in the codeflower://
// line (fp=…), next to the token, and clients accept the bridge only if its
// certificate has that fingerprint. Whoever can read the cluster's network
// then sees neither the chats nor the token, and cannot stand in for the
// bridge.
const (
	TLSCertVariable = "CODEFLOWER_TLS_CERT"
	TLSKeyVariable  = "CODEFLOWER_TLS_KEY"
)

// Certificate is a run's certificate and key (PEM), and its fingerprint.
type Certificate struct {
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
}

// NewCertificate makes a self-signed certificate for one run.
func NewCertificate() (Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "code flower worker"},
		// Clients check the fingerprint, not the names or dates; the dates
		// only need to cover the run, however long the job.
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.AddDate(10, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Certificate{}, err
	}
	return Certificate{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint: CertificateFingerprint(der),
	}, nil
}

// CertificateFingerprint is the fingerprint of a certificate (DER): its
// SHA-256, unpadded base64url, as the bridge and every client write it.
func CertificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidFingerprint says whether text is a fingerprint (32 bytes, base64url).
func ValidFingerprint(text string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(text)
	return err == nil && len(decoded) == sha256.Size
}

// PinnedTLS accepts only a server whose certificate has the fingerprint.
func PinnedTLS(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// No authority signs the bridge's certificate: VerifyConnection checks
		// it against the fingerprint instead, before any request is sent.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("the worker sent no certificate")
			}
			got := CertificateFingerprint(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(fingerprint)) != 1 {
				return errors.New("the worker's certificate does not match its address: it is not the worker that printed it")
			}
			return nil
		},
	}
}

// Encrypted says whether the bridge serves TLS (its line has a fingerprint).
func (c Connection) Encrypted() bool {
	return c.Fingerprint != ""
}

// BridgeURL is the address of one of the bridge's paths: https with a
// fingerprint, else (bridges that serve the page, older images) http.
func (c Connection) BridgeURL(path string) string {
	scheme := "http"
	if c.Encrypted() {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) + path
}

// Transport reaches the bridge straight (a cluster's HTTPS_PROXY cannot reach
// compute nodes), checking its certificate when it serves TLS.
func (c Connection) Transport() *http.Transport {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if c.Encrypted() {
		transport.TLSClientConfig = PinnedTLS(c.Fingerprint)
	}
	return transport
}

// Client is an HTTP client for the bridge, with Transport.
func (c Connection) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: c.Transport(), Timeout: timeout}
}
