// Package tlsutil makes the TLS configuration of the listeners: TLS 1.2 or later, and a certificate that is read again from its files when they change, so that a renewed certificate needs no restart.
package tlsutil

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

type reloader struct {
	cert, key string
	mu        sync.Mutex
	loaded    *tls.Certificate
	stamp     time.Time // the later of the two modification times of what was loaded
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, k := stat(r.cert), stat(r.key)
	latest := c
	if k.After(c) {
		latest = k
	}
	if r.loaded != nil && !latest.After(r.stamp) {
		return r.loaded, nil
	}
	pair, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		if r.loaded != nil {
			return r.loaded, nil // a certificate that is being replaced and is not whole yet: the one that was good goes on
		}
		return nil, err
	}
	r.loaded, r.stamp = &pair, latest
	return r.loaded, nil
}

func stat(p string) time.Time {
	if i, err := os.Stat(p); err == nil {
		return i.ModTime()
	}
	return time.Time{}
}

// Config makes the configuration for a server from a certificate file and a key file. It fails at once when they cannot be read.
func Config(certFile, keyFile string) (*tls.Config, error) {
	r := &reloader{cert: certFile, key: keyFile}
	if _, err := r.get(nil); err != nil {
		return nil, fmt.Errorf("the certificate and the key: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, nil
}
