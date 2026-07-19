package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
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

type targetTransportPool struct {
	mu                sync.Mutex
	transports        map[Target]*http.Transport
	tlsConfig         *tls.Config
	certificateSource ClientCertificateSource
	dialContext       func(context.Context, string, string) (net.Conn, error)
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
	transport := &targetTransportPool{transports: make(map[Target]*http.Transport), certificateSource: certificateSource, dialContext: config.DialContext, tlsConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: strings.TrimSpace(config.ServerName),
	}}
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
	if guest.CompareReadiness(status.Snapshot.Level, guest.ReadinessAllocated) < 0 {
		return ReadinessStatus{}, &TransportError{operation: readinessPath, message: "response contains an unknown readiness level", transient: false}
	}
	return status, nil
}

func (client *Client) ApplyProjectSeed(ctx context.Context, request ProjectSeedRequest) error {
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(encodeProjectSeedRequest(writer, request))
	}()
	defer reader.Close()
	return client.postReader(ctx, request.Target, projectSeedPath, reader, &emptyResponse{})
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
	return client.postReader(ctx, target, path, bytes.NewReader(body), output)
}

func (client *Client) postReader(ctx context.Context, target Target, path string, body io.Reader, output any) error {
	address, err := targetControlAddress(target, client.port)
	if err != nil {
		return &TransportError{operation: path, message: err.Error(), transient: false, cause: err}
	}
	endpoint := "https://" + address + path
	requestContext := context.WithValue(ctx, clientTargetContextKey{}, target)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("construct guest control request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		transient := !permanentConnectionError(err)
		if classifiedTransient, classified := ClassifyTransientError(err); classified {
			transient = classifiedTransient
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

// encodeProjectSeedRequest preserves the JSON contract while streaming each
// raw artifact through base64 directly to the HTTP request. Peak sender-side
// payload memory is the retained raw Seed plus bounded encoder buffers, not a
// second full base64-expanded JSON copy.
func encodeProjectSeedRequest(writer io.Writer, request ProjectSeedRequest) error {
	if _, err := io.WriteString(writer, `{"target":`); err != nil {
		return err
	}
	if err := writeJSONValue(writer, request.Target); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, `,"seed":{"RepositoryURL":`); err != nil {
		return err
	}
	if err := writeJSONValue(writer, request.Seed.RepositoryURL); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, `,"BaseRevision":`); err != nil {
		return err
	}
	if err := writeJSONValue(writer, request.Seed.BaseRevision); err != nil {
		return err
	}
	for _, artifact := range []struct {
		name  string
		value guest.ProjectSeedArtifact
	}{
		{name: "GitBundle", value: request.Seed.GitBundle},
		{name: "TrackedPatch", value: request.Seed.TrackedPatch},
		{name: "UntrackedTar", value: request.Seed.UntrackedTar},
		{name: "Manifest", value: request.Seed.Manifest},
	} {
		if _, err := fmt.Fprintf(writer, `,%q:`, artifact.name); err != nil {
			return err
		}
		if err := writeProjectSeedArtifact(writer, artifact.value); err != nil {
			return err
		}
	}
	_, err := io.WriteString(writer, `}}`)
	return err
}

func writeProjectSeedArtifact(writer io.Writer, artifact guest.ProjectSeedArtifact) error {
	if _, err := io.WriteString(writer, `{"SHA256":`); err != nil {
		return err
	}
	if err := writeJSONValue(writer, artifact.SHA256); err != nil {
		return err
	}
	if artifact.Content == nil {
		_, err := io.WriteString(writer, `,"Content":null}`)
		return err
	}
	if _, err := io.WriteString(writer, `,"Content":"`); err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, writer)
	if _, err := encoder.Write(artifact.Content); err != nil {
		_ = encoder.Close()
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(writer, `"}`)
	return err
}

func writeJSONValue(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(encoded)
	return err
}

type clientTargetContextKey struct{}

func (pool *targetTransportPool) RoundTrip(request *http.Request) (*http.Response, error) {
	target, ok := request.Context().Value(clientTargetContextKey{}).(Target)
	if !ok {
		return nil, errors.New("guest control request has no Target")
	}
	return pool.transport(target).RoundTrip(request)
}

func (pool *targetTransportPool) transport(target Target) *http.Transport {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if transport := pool.transports[target]; transport != nil {
		return transport
	}
	tlsConfig := pool.tlsConfig.Clone()
	tlsConfig.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certificate, err := pool.certificateSource.ClientCertificate(info.Context(), target)
		if err != nil {
			return nil, err
		}
		return &certificate, nil
	}
	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		MaxIdleConns:    64, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext:         pool.dialContext,
	}
	pool.transports[target] = transport
	return transport
}

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
