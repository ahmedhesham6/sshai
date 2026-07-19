package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type DirectoryClientCertificateSource struct {
	certificateDirectory string
	privateKeyDirectory  string
}

// NewDirectoryClientCertificateSource implements the provisioning contract
// for Environment-scoped workflow client certificates. Each directory holds
// <environment-id>.pem; private keys must satisfy the same trusted-0600 rules
// as the guest server key.
func NewDirectoryClientCertificateSource(certificateDirectory, privateKeyDirectory string) (*DirectoryClientCertificateSource, error) {
	certificateDirectory = filepath.Clean(certificateDirectory)
	privateKeyDirectory = filepath.Clean(privateKeyDirectory)
	if !filepath.IsAbs(certificateDirectory) || !filepath.IsAbs(privateKeyDirectory) {
		return nil, errors.New("client certificate directories must be absolute")
	}
	return &DirectoryClientCertificateSource{
		certificateDirectory: certificateDirectory,
		privateKeyDirectory:  privateKeyDirectory,
	}, nil
}

func (source *DirectoryClientCertificateSource) ClientCertificate(_ context.Context, target Target) (tls.Certificate, error) {
	environmentID := strings.TrimSpace(target.EnvironmentID)
	if source == nil || environmentID == "" || filepath.Base(environmentID) != environmentID || strings.ContainsAny(environmentID, `/\\`) {
		return tls.Certificate{}, permanentOperationError(errors.New("load Environment client certificate: Environment ID is invalid"))
	}
	certificateFile := filepath.Join(source.certificateDirectory, environmentID+".pem")
	privateKeyFile := filepath.Join(source.privateKeyDirectory, environmentID+".pem")
	certificate, err := loadTrustedX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return tls.Certificate{}, permanentOperationError(fmt.Errorf("load Environment client certificate: %w", err))
	}
	return certificate, nil
}

func LoadServerTLSConfig(certificateFile, privateKeyFile, clientCAFile string) (*tls.Config, error) {
	certificate, err := loadTrustedX509KeyPair(certificateFile, privateKeyFile)
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

func loadTrustedX509KeyPair(certificateFile, privateKeyFile string) (tls.Certificate, error) {
	certificatePEM, err := readTrustedPublicFile(certificateFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	privateKeyPEM, err := readTrustedPrivateKey(privateKeyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	return certificate, nil
}

func readTrustedPublicFile(name string) ([]byte, error) {
	return readTrustedFile(name, 16<<20, func(stat unix.Stat_t) error {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o022 != 0 {
			return errors.New("TLS public material must be a non-writable regular file")
		}
		trustedUID := uint32(os.Geteuid())
		if stat.Uid != 0 && stat.Uid != trustedUID {
			return fmt.Errorf("TLS public material owner UID %d is not trusted", stat.Uid)
		}
		return nil
	})
}

func readTrustedPrivateKey(name string) ([]byte, error) {
	return readTrustedFile(name, 1<<20, func(stat unix.Stat_t) error {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 {
			return errors.New("private key must be a 0600 regular file")
		}
		trustedUID := uint32(os.Geteuid())
		if stat.Uid != 0 && stat.Uid != trustedUID {
			return fmt.Errorf("private key owner UID %d is not trusted", stat.Uid)
		}
		return nil
	})
}

func readTrustedFile(name string, maximum int64, validate func(unix.Stat_t) error) ([]byte, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, errors.New("private key file descriptor is invalid")
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if err := validate(stat); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maximum {
		return nil, fmt.Errorf("TLS file exceeds %d bytes", maximum)
	}
	return content, nil
}

func loadCertificatePool(name string) (*x509.CertPool, error) {
	if name == "" {
		return nil, errors.New("CA file is required")
	}
	content, err := readTrustedPublicFile(name)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(content) {
		return nil, errors.New("CA file contains no certificates")
	}
	return pool, nil
}
