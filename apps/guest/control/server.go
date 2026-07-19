package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ServerConfig struct {
	Target          Target
	ClientIdentity  string
	MaxRequestBytes int64
	QuiesceTimeout  time.Duration
}

type server struct {
	config     ServerConfig
	operations Operations
	stateMu    sync.Mutex
	quiescing  bool
	inFlight   int
	idle       chan struct{}
}

func NewServer(config ServerConfig, operations Operations) (http.Handler, error) {
	for name, value := range map[string]string{
		"owner": config.Target.OwnerUserID, "Environment": config.Target.EnvironmentID,
		"Runtime": config.Target.RuntimeID, "provider": config.Target.ProviderID, "private address": config.Target.PrivateIPv4,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("construct guest control server: %s identity is required", name)
		}
	}
	claimedEnvironment, err := clientIdentityEnvironment(config.ClientIdentity)
	if err != nil {
		return nil, errors.New("construct guest control server: client URI identity is required")
	}
	if subtle.ConstantTimeCompare([]byte(claimedEnvironment), []byte(config.Target.EnvironmentID)) != 1 {
		return nil, errors.New("construct guest control server: client URI identity must claim this Environment")
	}
	if operations == nil {
		return nil, errors.New("construct guest control server: operations are required")
	}
	if config.MaxRequestBytes <= 0 {
		config.MaxRequestBytes = defaultMaximumRequestBytes
	}
	if config.QuiesceTimeout <= 0 {
		config.QuiesceTimeout = 10 * time.Minute
	}
	return &server{config: config, operations: operations}, nil
}

func clientIdentityEnvironment(identity string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(identity))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("client URI identity is invalid")
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(segments) < 2 || segments[len(segments)-2] != "environment" {
		return "", errors.New("client URI identity has no Environment claim")
	}
	value, err := url.PathUnescape(segments[len(segments)-1])
	if err != nil || strings.TrimSpace(value) == "" || strings.Contains(value, "/") {
		return "", errors.New("client URI identity Environment claim is invalid")
	}
	return value, nil
}

func (server *server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeError(response, http.StatusMethodNotAllowed, errors.New("method is not allowed"))
		return
	}
	if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
		writeError(response, http.StatusUnauthorized, errors.New("verified mutual TLS is required"))
		return
	}
	if !server.authorizePeer(response, request) {
		return
	}
	if isMutatingPath(request.URL.Path) && server.isQuiescing() {
		writeError(response, http.StatusConflict, errors.New("guest is quiescing for Runtime shutdown"))
		return
	}
	switch request.URL.Path {
	case readinessPath:
		var input ReadinessRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		result, err := server.operations.Readiness(request.Context(), input.Target)
		server.writeResult(response, result, err)
	case projectSeedPath:
		var input ProjectSeedRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		if !server.beginMutation(response) {
			return
		}
		defer server.endMutation()
		err := server.operations.ApplyProjectSeed(request.Context(), input)
		server.writeResult(response, emptyResponse{}, err)
	case sshHostIdentityPath:
		var input targetRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		if !server.beginMutation(response) {
			return
		}
		defer server.endMutation()
		status, err := server.operations.RestoreSSHHostIdentity(request.Context(), input.Target)
		server.writeResult(response, sshHostIdentityResponse{Status: status}, err)
	case sshKeysPath:
		server.runTargetOperation(response, request, true, server.operations.ReconcileSSHKeys)
	case managedConfigurationPath:
		server.runTargetOperation(response, request, true, server.operations.ReconcileManagedConfiguration)
	case shutdownPath:
		server.runShutdown(response, request)
	case materializationPath:
		var input MaterializationRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		if !server.beginMutation(response) {
			return
		}
		defer server.endMutation()
		results, err := server.operations.ApplyMaterialization(request.Context(), input)
		server.writeResult(response, materializationResponse{Results: results}, err)
	case toolchainValidationPath:
		server.runTargetOperation(response, request, false, server.operations.ValidateToolchain)
	case activitySnapshotPath:
		var input targetRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		snapshot, err := server.operations.ReadActivitySnapshot(request.Context(), input.Target)
		server.writeResult(response, activitySnapshotResponse{Snapshot: snapshot}, err)
	default:
		writeError(response, http.StatusNotFound, errors.New("control operation was not found"))
	}
}

func isMutatingPath(path string) bool {
	switch path {
	case projectSeedPath, sshHostIdentityPath, sshKeysPath, managedConfigurationPath, materializationPath:
		return true
	default:
		return false
	}
}

func (server *server) isQuiescing() bool {
	server.stateMu.Lock()
	defer server.stateMu.Unlock()
	return server.quiescing
}

func (server *server) authorizePeer(response http.ResponseWriter, request *http.Request) bool {
	for _, identity := range request.TLS.PeerCertificates[0].URIs {
		if subtle.ConstantTimeCompare([]byte(identity.String()), []byte(server.config.ClientIdentity)) == 1 {
			return true
		}
	}
	writeError(response, http.StatusForbidden, errors.New("client certificate identity is not authorized"))
	return false
}

func (server *server) runTargetOperation(response http.ResponseWriter, request *http.Request, mutating bool, operation func(context.Context, Target) error) {
	var input targetRequest
	if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
		return
	}
	if mutating && !server.beginMutation(response) {
		return
	}
	if mutating {
		defer server.endMutation()
	}
	server.writeResult(response, emptyResponse{}, operation(request.Context(), input.Target))
}

func (server *server) runShutdown(response http.ResponseWriter, request *http.Request) {
	var input targetRequest
	if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
		return
	}
	if err := server.quiesce(request.Context()); err != nil {
		server.writeResult(response, emptyResponse{}, transientOperationError(fmt.Errorf("quiesce guest mutations: %w", err)))
		return
	}
	server.writeResult(response, emptyResponse{}, server.operations.PrepareShutdown(request.Context(), input.Target))
}

func (server *server) beginMutation(response http.ResponseWriter) bool {
	server.stateMu.Lock()
	defer server.stateMu.Unlock()
	if server.quiescing {
		writeError(response, http.StatusConflict, errors.New("guest is quiescing for Runtime shutdown"))
		return false
	}
	if server.inFlight == 0 {
		server.idle = make(chan struct{})
	}
	server.inFlight++
	return true
}

func (server *server) endMutation() {
	server.stateMu.Lock()
	defer server.stateMu.Unlock()
	server.inFlight--
	if server.inFlight == 0 && server.idle != nil {
		close(server.idle)
	}
}

func (server *server) quiesce(ctx context.Context) error {
	server.stateMu.Lock()
	server.quiescing = true
	if server.inFlight == 0 {
		server.stateMu.Unlock()
		return nil
	}
	idle := server.idle
	server.stateMu.Unlock()
	waitContext, cancel := context.WithTimeout(ctx, server.config.QuiesceTimeout)
	defer cancel()
	select {
	case <-idle:
		return nil
	case <-waitContext.Done():
		return waitContext.Err()
	}
}

func (server *server) decode(response http.ResponseWriter, request *http.Request, destination any) bool {
	if contentType := request.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		writeError(response, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, server.config.MaxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, errors.New("request must contain exactly one JSON document"))
		return false
	}
	return true
}

func (server *server) authorize(response http.ResponseWriter, target Target) bool {
	for _, identity := range []struct{ expected, actual string }{
		{server.config.Target.OwnerUserID, target.OwnerUserID},
		{server.config.Target.EnvironmentID, target.EnvironmentID},
		{server.config.Target.RuntimeID, target.RuntimeID},
		{server.config.Target.ProviderID, target.ProviderID},
		{server.config.Target.PrivateIPv4, target.PrivateIPv4},
	} {
		if subtle.ConstantTimeCompare([]byte(identity.expected), []byte(identity.actual)) != 1 {
			writeError(response, http.StatusForbidden, errors.New("request does not target this Environment's current Runtime"))
			return false
		}
	}
	return true
}

func (server *server) writeResult(response http.ResponseWriter, result any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if transient, classified := ClassifyTransientError(err); classified {
			if transient {
				status = http.StatusServiceUnavailable
			} else {
				status = http.StatusUnprocessableEntity
			}
		}
		failure := errorResponse{Error: err.Error()}
		if materialization, ok := result.(materializationResponse); ok {
			failure.Results = materialization.Results
		}
		writeJSONError(response, status, failure)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(result)
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSONError(response, status, errorResponse{Error: err.Error()})
}

func writeJSONError(response http.ResponseWriter, status int, failure errorResponse) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(failure)
}
