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
)

type ServerConfig struct {
	EnvironmentID   string
	ClientIdentity  string
	MaxRequestBytes int64
}

type server struct {
	config     ServerConfig
	operations Operations
}

func NewServer(config ServerConfig, operations Operations) (http.Handler, error) {
	config.EnvironmentID = strings.TrimSpace(config.EnvironmentID)
	if config.EnvironmentID == "" {
		return nil, errors.New("construct guest control server: Environment ID is required")
	}
	if _, err := url.ParseRequestURI(config.ClientIdentity); err != nil || !strings.Contains(config.ClientIdentity, "://") {
		return nil, errors.New("construct guest control server: client URI identity is required")
	}
	if operations == nil {
		return nil, errors.New("construct guest control server: operations are required")
	}
	if config.MaxRequestBytes <= 0 {
		config.MaxRequestBytes = defaultMaximumRequestBytes
	}
	return &server{config: config, operations: operations}, nil
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
		err := server.operations.ApplyProjectSeed(request.Context(), input)
		server.writeResult(response, emptyResponse{}, err)
	case sshHostIdentityPath:
		var input targetRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		status, err := server.operations.RestoreSSHHostIdentity(request.Context(), input.Target)
		server.writeResult(response, sshHostIdentityResponse{Status: status}, err)
	case sshKeysPath:
		server.runTargetOperation(response, request, server.operations.ReconcileSSHKeys)
	case managedConfigurationPath:
		server.runTargetOperation(response, request, server.operations.ReconcileManagedConfiguration)
	case shutdownPath:
		server.runTargetOperation(response, request, server.operations.PrepareShutdown)
	case materializationPath:
		var input MaterializationRequest
		if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
			return
		}
		results, err := server.operations.ApplyMaterialization(request.Context(), input)
		server.writeResult(response, materializationResponse{Results: results}, err)
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

func (server *server) authorizePeer(response http.ResponseWriter, request *http.Request) bool {
	for _, identity := range request.TLS.PeerCertificates[0].URIs {
		if subtle.ConstantTimeCompare([]byte(identity.String()), []byte(server.config.ClientIdentity)) == 1 {
			return true
		}
	}
	writeError(response, http.StatusForbidden, errors.New("client certificate identity is not authorized"))
	return false
}

func (server *server) runTargetOperation(response http.ResponseWriter, request *http.Request, operation func(context.Context, Target) error) {
	var input targetRequest
	if !server.decode(response, request, &input) || !server.authorize(response, input.Target) {
		return
	}
	server.writeResult(response, emptyResponse{}, operation(request.Context(), input.Target))
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
	if target.EnvironmentID == "" || subtle.ConstantTimeCompare([]byte(target.EnvironmentID), []byte(server.config.EnvironmentID)) != 1 {
		writeError(response, http.StatusForbidden, errors.New("request does not target this Environment"))
		return false
	}
	return true
}

func (server *server) writeResult(response http.ResponseWriter, result any, err error) {
	if err != nil {
		writeError(response, http.StatusUnprocessableEntity, err)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(result)
}

func writeError(response http.ResponseWriter, status int, err error) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(errorResponse{Error: err.Error()})
}
