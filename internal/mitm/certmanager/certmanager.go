package certmanager

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Manager handles generation and caching of TLS certificates for MITM interception.
type Manager struct {
	dataDir string
	caCert  *x509.Certificate
	caKey   *ecdsa.PrivateKey

	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// New creates a Manager that stores the CA in dataDir.
// It loads an existing CA or generates a new one on first use.
func New(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("certmanager: create data dir: %w", err)
	}
	m := &Manager{
		dataDir: dataDir,
		cache:   make(map[string]*tls.Certificate),
	}
	if err := m.loadOrCreateCA(); err != nil {
		return nil, err
	}
	return m, nil
}

// CACertPEM returns the CA certificate in PEM format (for injecting into child process trust stores).
func (m *Manager) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: m.caCert.Raw,
	})
}

// CACertPath writes the CA cert to a temp file and returns the path.
// Suitable for setting SSL_CERT_FILE on child processes.
func (m *Manager) CACertPath() (string, error) {
	p := filepath.Join(m.dataDir, "mitm-ca.crt")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	systemCerts, err := x509.SystemCertPool()
	if err != nil || systemCerts == nil {
		systemCerts = x509.NewCertPool()
	}

	var bundle []byte
	for _, pemCert := range extractSystemPEMs(systemCerts) {
		bundle = append(bundle, pemCert...)
	}
	bundle = append(bundle, m.CACertPEM()...)

	if err := os.WriteFile(p, bundle, 0o600); err != nil {
		return "", fmt.Errorf("certmanager: write CA bundle: %w", err)
	}
	return p, nil
}

// GetCertificate returns a TLS certificate for the given hostname,
// generating and caching one on the fly if needed.
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		host = "localhost"
	}

	m.mu.RLock()
	if cert, ok := m.cache[host]; ok {
		m.mu.RUnlock()
		return cert, nil
	}
	m.mu.RUnlock()

	cert, err := m.issueCert(host)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache[host] = cert
	m.mu.Unlock()

	return cert, nil
}

// TLSConfig returns a *tls.Config suitable for the MITM proxy's listener.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
}

func (m *Manager) loadOrCreateCA() error {
	certPath := filepath.Join(m.dataDir, "ca-cert.pem")
	keyPath := filepath.Join(m.dataDir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)

	if certErr == nil && keyErr == nil {
		return m.parseCA(certPEM, keyPEM)
	}

	return m.generateCA(certPath, keyPath)
}

func (m *Manager) parseCA(certPEM, keyPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("certmanager: failed to decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("certmanager: parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("certmanager: failed to decode CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("certmanager: parse CA key: %w", err)
	}

	m.caCert = cert
	m.caKey = key
	return nil
}

func (m *Manager) generateCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("certmanager: generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("certmanager: generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"CLIProxyAPI MITM"},
			CommonName:   "CLIProxyAPI MITM CA",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("certmanager: create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return fmt.Errorf("certmanager: parse generated CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("certmanager: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("certmanager: write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("certmanager: write CA key: %w", err)
	}

	m.caCert = cert
	m.caKey = key
	return nil
}

func (m *Manager) issueCert(hostname string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certmanager: generate host key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certmanager: generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, fmt.Errorf("certmanager: create host cert: %w", err)
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certDER, m.caCert.Raw},
		PrivateKey:  key,
	}

	return tlsCert, nil
}

// extractSystemPEMs is a best-effort extraction of PEM blocks from the system pool.
// On platforms where this isn't possible, we just return our own CA.
func extractSystemPEMs(pool *x509.CertPool) [][]byte {
	if pool == nil {
		return nil
	}
	var pems [][]byte
	for _, s := range pool.Subjects() { //nolint:staticcheck
		_ = s // we iterate to get a count but can't extract PEM from Subjects()
	}
	// The standard library doesn't expose individual PEM blocks from the pool.
	// Instead, we rely on the system's default bundle file.
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/cert.pem",
	} {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			pems = append(pems, data)
			break
		}
	}
	return pems
}
