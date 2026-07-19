package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

const defaultConnectionIntentTTL = 60 * time.Second

func (server *server) CreateConnectionIntent(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID, params contracts.CreateConnectionIntentParams) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	detail, err := server.environmentReads.GetOwnedEnvironment(request.Context(), user.ID, string(environmentID))
	if err != nil {
		server.writeRuntimeCommandError(response, request, err)
		return
	}
	proxyBase, present := server.regionalProxyURLs[detail.Environment.Snapshot().Region]
	if !present {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The regional SSH proxy is unavailable.")
		return
	}
	if server.connectionIntentIDs == nil || server.now == nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The connection intent could not be created safely.")
		return
	}

	operationID := detail.ActiveOperationID
	if detail.Runtime == nil || !runtimeAlreadyConnecting(detail.Runtime.Snapshot().Status) {
		if server.runtimeCommands == nil {
			writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Runtime could not be started safely.")
			return
		}
		command, err := server.runtimeCommands.StartRuntime(request.Context(), application.RuntimeCommandInput{
			OwnerUserID: user.ID, EnvironmentID: string(environmentID), IdempotencyKey: string(params.IdempotencyKey),
		})
		if err != nil {
			server.writeRuntimeCommandError(response, request, err)
			return
		}
		startedOperationID := command.Operation().Snapshot().ID
		operationID = &startedOperationID
	}

	now := server.now().UTC()
	result := contracts.CreateConnectionIntent201JSONResponse{
		Headers: contracts.CreateConnectionIntent201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body: contracts.ConnectionIntent{
			Id: server.connectionIntentIDs.NewID(), EnvironmentId: string(environmentID),
			LogicalHostname: string(environmentID), ProxyUrl: connectionProxyURL(proxyBase, string(environmentID)),
			OperationId: operationID, ExpiresAt: now.Add(server.connectionIntentTTL),
		},
	}
	if err := result.VisitCreateConnectionIntentResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func runtimeAlreadyConnecting(status domain.RuntimeStatus) bool {
	return status == domain.RuntimeReady || status == domain.RuntimeStarting
}

func connectionProxyURL(base *url.URL, environmentID string) string {
	proxyURL := *base
	proxyURL.Path = "/v1/environments/" + environmentID + "/ssh"
	proxyURL.RawPath = ""
	return proxyURL.String()
}

// ValidateRegionalProxyURLs checks the regional WSS endpoints accepted by
// Config. A missing Environment region remains a request-time dependency
// error, so an empty map is valid.
func ValidateRegionalProxyURLs(regionalURLs map[string]string) error {
	_, err := parseRegionalProxyURLs(regionalURLs)
	return err
}

func parseRegionalProxyURLs(regionalURLs map[string]string) (map[string]*url.URL, error) {
	parsedURLs := make(map[string]*url.URL, len(regionalURLs))
	for region, rawURL := range regionalURLs {
		if strings.TrimSpace(region) == "" || region != strings.TrimSpace(region) {
			return nil, errors.New("regional proxy URL has an invalid region")
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
			return nil, fmt.Errorf("regional proxy URL for %q must be WSS without credentials, query, or fragment", region)
		}
		parsedURLs[region] = parsed
	}
	return parsedURLs, nil
}
