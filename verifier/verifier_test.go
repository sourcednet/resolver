package verifier

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// caPEM returns a freshly generated self-signed CA certificate in PEM form.
func caPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestHTTPClientCA(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ca.pem")
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(good, caPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := HTTPClient(good, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != time.Second {
		t.Fatalf("timeout %s", c.Timeout)
	}
	if _, err := HTTPClient(bad, time.Second); err == nil {
		t.Fatal("a file without certificates should fail")
	}
	if _, err := HTTPClient(filepath.Join(dir, "missing.pem"), time.Second); err == nil {
		t.Fatal("a missing file should fail")
	}
	if _, err := HTTPClient("", 0); err != nil {
		t.Fatal(err)
	}
}
