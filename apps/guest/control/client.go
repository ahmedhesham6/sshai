package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

type ClientConfig struct {
	Endpoint        string
	CertificateFile string
	PrivateKeyFile  string
	CAFile          string
	PollInterval    time.Duration
	RequestTimeout  time.Duration
}

type Client struct {
	endpoint     *url.URL
	http         *http.Client
	pollInterval time.Duration
}

type TransportError struct {
	operation string
	message   string
	transient bool
}

func (err *TransportError) Error() string {
	return "guest control " + err.operation + ": " + err.message
}

func (err *TransportError) Transient() bool { return err.transient }

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("construct guest control client: Endpoint must be an HTTPS origin")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if config.CertificateFile == "" || config.PrivateKeyFile == "" || config.CAFile == "" {
		return nil, errors.New("construct guest control client: certificate, private key, and CA files are required")
	}
	certificate, err := tls.LoadX509KeyPair(config.CertificateFile, config.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("construct guest control client: load client certificate: %w", err)
	}
	roots, err := loadCertificatePool(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("construct guest control client: load server CA: %w", err)
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Minute
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
	}}
	return &Client{
		endpoint:     endpoint,
		http:         &http.Client{Transport: transport, Timeout: requestTimeout},
		pollInterval: pollInterval,
	}, nil
}

func (client *Client) WaitForReadiness(ctx context.Context, target Target, minimum guest.ReadinessLevel) (ReadinessStatus, error) {
	minimumOrder := readinessOrder(minimum)
	if minimumOrder == 0 {
		return ReadinessStatus{}, errors.New("wait for guest readiness: minimum readiness level is invalid")
	}
	for {
		var status ReadinessStatus
		if err := client.post(ctx, readinessPath, ReadinessRequest{Target: target}, &status); err != nil {
			return ReadinessStatus{}, err
		}
		if readinessOrder(status.Snapshot.Level) >= minimumOrder {
			return status, nil
		}
		timer := time.NewTimer(client.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ReadinessStatus{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (client *Client) ApplyProjectSeed(ctx context.Context, request ProjectSeedRequest) error {
	return client.post(ctx, projectSeedPath, request, &emptyResponse{})
}

func (client *Client) RestoreSSHHostIdentity(ctx context.Context, target Target) (guest.SSHHostIdentityStatus, error) {
	var response sshHostIdentityResponse
	err := client.post(ctx, sshHostIdentityPath, targetRequest{Target: target}, &response)
	return response.Status, err
}

func (client *Client) ReconcileSSHKeys(ctx context.Context, target Target) error {
	return client.post(ctx, sshKeysPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) ReconcileManagedConfiguration(ctx context.Context, target Target) error {
	return client.post(ctx, managedConfigurationPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) PrepareShutdown(ctx context.Context, target Target) error {
	return client.post(ctx, shutdownPath, targetRequest{Target: target}, &emptyResponse{})
}

func (client *Client) ApplyMaterialization(ctx context.Context, request MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	var response materializationResponse
	err := client.post(ctx, materializationPath, request, &response)
	return response.Results, err
}

func (client *Client) ReadActivitySnapshot(ctx context.Context, target Target) (ActivitySnapshot, error) {
	var response activitySnapshotResponse
	err := client.post(ctx, activitySnapshotPath, targetRequest{Target: target}, &response)
	return response.Snapshot, err
}

func (client *Client) post(ctx context.Context, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode guest control request: %w", err)
	}
	endpoint := *client.endpoint
	endpoint.Path += path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construct guest control request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return &TransportError{operation: path, message: err.Error(), transient: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message := http.StatusText(response.StatusCode)
		var failure errorResponse
		decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
		if decoder.Decode(&failure) == nil && failure.Error != "" {
			message = failure.Error
		}
		return &TransportError{operation: path, message: message, transient: response.StatusCode >= 500}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, defaultMaximumRequestBytes))
	if err := decoder.Decode(output); err != nil {
		return &TransportError{operation: path, message: "decode response: " + err.Error(), transient: true}
	}
	return nil
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
