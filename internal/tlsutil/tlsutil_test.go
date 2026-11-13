package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeCert(t *testing.T, dir, cn string) (certFile, keyFile string, pool *x509.CertPool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return
}

func get(t *testing.T, url string, cfg *tls.Config) error {
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get(url)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// TLS 1.2 is the least; a certificate that is verified; a renewed certificate is served without a restart.
func TestConfig(t *testing.T) {
	dir := t.TempDir()
	cert, key, pool := makeCert(t, dir, "one")
	cfg, err := Config(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	defer srv.Close()
	url := "https://" + ln.Addr().String()
	if err := get(t, url, &tls.Config{RootCAs: pool}); err != nil {
		t.Fatalf("a client that trusts the certificate: %v", err)
	}
	if err := get(t, url, &tls.Config{}); err == nil {
		t.Fatal("a client that does not trust the certificate was served")
	}
	if err := get(t, url, &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}); err == nil {
		t.Fatal("TLS 1.1 was served")
	}
	// the certificate is replaced on disk: the new one is served at the next handshake
	time.Sleep(20 * time.Millisecond)
	cert2, key2, pool2 := makeCert(t, dir, "two")
	if cert2 != cert || key2 != key {
		t.Fatal("the files are not the same files")
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(cert, future, future)
	if err := get(t, url, &tls.Config{RootCAs: pool2}); err != nil {
		t.Fatalf("the renewed certificate is not served: %v", err)
	}
	// a half-written replacement does not take the server down
	_ = os.WriteFile(cert, []byte("not a certificate"), 0o600)
	later := time.Now().Add(2 * time.Minute)
	_ = os.Chtimes(cert, later, later)
	if err := get(t, url, &tls.Config{RootCAs: pool2}); err != nil {
		t.Fatalf("the certificate that was good is not served while the new one is not whole: %v", err)
	}
	if _, err := Config("/no/cert", "/no/key"); err == nil {
		t.Fatal("files that are not there")
	}
}
