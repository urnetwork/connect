package connect

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	_ "embed"
)

// the let's encrypt root CAs as defined at https://letsencrypt.org/certificates/
// this includes:
// - ISRG Root X1
// - ISRG Root X2
//
//go:embed net_tls_ca.pem
var tlsCaPem string

func PinnedCertPool() (*x509.CertPool, error) {

	certPool := x509.NewCertPool()

	if !certPool.AppendCertsFromPEM([]byte(tlsCaPem)) {
		return nil, fmt.Errorf("Could not append ca certs")
	}

	return certPool, nil
}

func DefaultTlsConfig() (*tls.Config, error) {
	certPool, err := PinnedCertPool()
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}
	return tlsConfig, nil
}
