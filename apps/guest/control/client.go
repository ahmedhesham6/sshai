package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

type ClientConfig struct {
	// Port is the guest control port. The destination address always comes
	// from each request Target; a process-global guest endpoint is forbidden.
	Port int
	// ServerName optionally pins the DNS identity used to authenticate each
	// per-Environment server certificate. When empty, the target private IPv4
	// address must be present in that certificate's IP SANs.
	ServerName        string
	CertificateFile   string
	PrivateKeyFile    string
	CertificateSource ClientCertificateSource
	CAFile            string
	RequestTimeout    time.Duration
	DialContext       func(context.Context, string, string) (net.Conn, error)
}

type ClientCertificateSource interface {
	ClientCertificate(context.Context, Target) (tls.Certificate, error)
}

type Client struct {
	port int
	http *http.Client
}

type TransportError struct {
	operation string
	message   string
	transient bool
	cause     error
}

func (err *TransportError) Error() string {
	return "guest control " + err.operation + ": " + err.message
}

func (err *TransportError) Transient() bool { return err.transient }

func (err *TransportError) Unwrap() error { return err.cause }

func NewClient(config ClientConfig) (*Client, error) {
	if config.CAFile == "" {
		return nil, errors.New("construct guest control client: CA file is required")
	}
	if config.Port == 0 {
		config.Port = 9443
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("construct guest control client: control port must be between 1 and 65535")
	}
	certificateSource := config.CertificateSource
	if certificateSource == nil {
		if config.CertificateFile == "" || config.PrivateKeyFile == "" {
			return nil, errors.New("construct guest control client: certificate source or certificate and private key files are required")
		}
		certificate, err := loadTrustedX509KeyPair(config.CertificateFile, config.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("construct guest control client: load client certificate: %w", err)
		}
		certificateSource = staticClientCertificateSource{certificate: certificate}
	}
	roots, err := loadCertificatePool(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("construct guest control client: load server CA: %w", err)
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Minute
	}
	transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: strings.TrimSpace(config.ServerName),
		GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			target, ok := info.Context().Value(clientTargetContextKey{}).(Target)
			if !ok {
				return nil, errors.New("guest control TLS handshake has no request Target")
			}
			certificate, err := certificateSource.ClientCertificate(info.Context(), target)
			if err != nil {
				return nil, err
			}
			return &certificate, nil
		},
	}}
	if config.DialContext != nil {
		transport.DialContext = config.DialContext
	}
	return &Client{
		port: config.Port,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// ReadReadiness returns exactly one current observation. Durable workflow
// polling owns all deadline, retry, and minimum-level decisions.
func (client *Client) ReadReadiness(ctx context.Context, target Target) (ReadinessStatus, error) {
	var status ReadinessStatus
	if err := client.post(ctx, target, readinessPath, ReadinessRequest{Target: target}, &status); err != nil {
		return ReadinessStatus{}, err
	}
	return status, nil
}

func (client *Client) ApplyProjectSeed(ctx context.Context, request ProjectSeedRequest) error {
	return client.post(ctx, request.Target, projectSeedPath, request, &emptyResponse{})
}

func (client *Client) RestoreSSHHostIdentity(ctx context.Context, target Target) (guest.SSHHostIdentityStatus, error) {
	var response sshHostIdentityResponse
	err := client.post(ctx, target, sshHostIdentityPath, targetRequest{Target: target}, &response)
	return response.Status, err
}

func (client *Client) ReconcileSSHKeys(ctx context.Context, target Target) error {
	return client.post(ctx, target, sshKeysPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) ReconcileManagedConfiguration(ctx context.Context, target Target) error {
	return client.post(ctx, target, managedConfigurationPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) PrepareShutdown(ctx context.Context, target Target) error {
	return client.post(ctx, target, shutdownPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) ApplyMaterialization(ctx context.Context, request MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	var response materializationResponse
	err := client.post(ctx, request.Target, materializationPath, request, &response)
	return response.Results, err
}

func (client *Client) ValidateToolchain(ctx context.Context, target Target) error {
	return client.post(ctx, target, toolchainValidationPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) ReadActivitySnapshot(ctx context.Context, target Target) (ActivitySnapshot, error) {
	var response activitySnapshotResponse
	err := client.post(ctx, target, activitySnapshotPath, targetRequest{Target: target}, &response)
	return response.Snapshot, err
}

func (client *Client) post(ctx context.Context, target Target, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode guest control request: %w", err)
	}
	address, err := targetControlAddress(target, client.port)
	if err != nil {
		return &TransportError{operation: path, message: err.Error(), transient: false, cause: err}
	}
	endpoint := "https://" + address + path
	requestContext := context.WithValue(ctx, clientTargetContextKey{}, target)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construct guest control request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		transient := !permanentConnectionError(err)
		var classified interface{ Transient() bool }
		if errors.As(err, &classified) {
			transient = classified.Transient()
		}
		return &TransportError{operation: path, message: err.Error(), transient: transient, cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message := http.StatusText(response.StatusCode)
		var failure errorResponse
		content, readErr := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
		if readErr == nil && len(content) <= 32<<20 {
			_ = json.Unmarshal(content, output)
		}
		if json.Unmarshal(content, &failure) == nil && failure.Error != "" {
			message = failure.Error
		}
		transient := response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests
		return &TransportError{operation: path, message: message, transient: transient}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, defaultMaximumRequestBytes))
	if err := decoder.Decode(output); err != nil {
		return &TransportError{operation: path, message: "decode response: " + err.Error(), transient: true}
	}
	return nil
}

type clientTargetContextKey struct{}

type staticClientCertificateSource struct{ certificate tls.Certificate }

func (source staticClientCertificateSource) ClientCertificate(context.Context, Target) (tls.Certificate, error) {
	return source.certificate, nil
}

func targetControlAddress(target Target, port int) (string, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(target.PrivateIPv4))
	if err != nil || !address.Is4() || (!address.IsPrivate() && !address.IsLoopback()) {
		return "", errors.New("target Runtime private IPv4 address is required")
	}
	return net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
}

func permanentConnectionError(err error) bool {
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var alert tls.AlertError
	if errors.As(err, &verification) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &alert) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "tls:") && (strings.Contains(message, "certificate") ||
		strings.Contains(message, "unknown authority") || strings.Contains(message, "hostname"))
}

func readinessOrder(level guest.ReadinessLevel) int {
	switch level {
	case guest.ReadinessAllocated:
		return 1
	case guest.ReadinessDataMounted:
		return 2
	case guest.ReadinessSSHReady:
		return 3
	case guest.ReadinessProjectReady:
		return 4
	case guest.ReadinessAgentsValidated:
		return 5
	default:
		return 0
	}
}
