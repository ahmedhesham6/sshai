package control

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

func LoadServerTLSConfig(certificateFile, privateKeyFile, clientCAFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load guest server certificate: %w", err)
	}
	clientCAs, err := loadCertificatePool(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("load guest client CA: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

func loadCertificatePool(name string) (*x509.CertPool, error) {
	if name == "" {
		return nil, errors.New("CA file is required")
	}
	content, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(content) {
		return nil, errors.New("CA file contains no certificates")
	}
	return pool, nil
}
