package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/connection"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
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
		server.writeRuntimeCommandError(response, request, err, nil)
		return
	}
	proxyBase, present := server.regionalProxyURLs[detail.Environment.Snapshot().Region]
	if !present {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The regional SSH proxy is unavailable.")
		return
	}
	if server.connectionIntentIDs == nil || server.connectionIntents == nil || server.now == nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The connection intent could not be created safely.")
		return
	}
	now := server.now().UTC()
	expiresAt := now.Add(server.connectionIntentTTL)
	record, err := server.connectionIntents.CreateOrReplayConnectionIntent(
		request.Context(), user.ID, string(params.IdempotencyKey), string(environmentID), now, expiresAt,
		func(ctx context.Context) (*string, error) {
			return server.prepareConnectionIntentStart(ctx, user.ID, detail, string(params.IdempotencyKey), expiresAt)
		},
		server.connectionIntentIDs.NewID,
	)
	if err != nil {
		server.writeRuntimeCommandError(response, request, err, detail.ActiveOperationID)
		return
	}
	result := contracts.CreateConnectionIntent201JSONResponse{
		Headers: contracts.CreateConnectionIntent201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body: contracts.ConnectionIntent{
			Id: record.IntentID, EnvironmentId: string(environmentID),
			LogicalHostname: string(environmentID), ProxyUrl: connection.ProxyURL(proxyBase, string(environmentID)),
			OperationId: record.OperationID, ExpiresAt: record.ExpiresAt,
		},
	}
	if err := result.VisitCreateConnectionIntentResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) prepareConnectionIntentStart(ctx context.Context, ownerID string, detail db.EnvironmentDetail, clientKey string, expiresAt time.Time) (*string, error) {
	environmentID := detail.Environment.Snapshot().ID
	if detail.ActiveOperationID != nil {
		return server.joinConnectionStart(ctx, ownerID, environmentID, *detail.ActiveOperationID)
	}
	if detail.Runtime != nil && detail.Runtime.Snapshot().Status == domain.RuntimeReady {
		return nil, nil
	}
	if detail.Runtime != nil && detail.Runtime.Snapshot().Status == domain.RuntimeStarting {
		return nil, domain.ErrRuntimeCommandState
	}
	if server.runtimeCommands == nil {
		return nil, errors.New("Connection Intent Runtime start is unavailable")
	}
	command, err := server.runtimeCommands.StartRuntime(ctx, application.RuntimeCommandInput{
		OwnerUserID: ownerID, EnvironmentID: environmentID,
		IdempotencyKey: connectionStartIdempotencyKey(ownerID, environmentID, clientKey, expiresAt),
	})
	if errors.Is(err, db.ErrOperationConflict) {
		detail, readErr := server.environmentReads.GetOwnedEnvironment(ctx, ownerID, environmentID)
		if readErr != nil {
			return nil, readErr
		}
		if detail.ActiveOperationID == nil {
			if detail.Runtime != nil && detail.Runtime.Snapshot().Status == domain.RuntimeReady {
				return nil, nil
			}
			return nil, domain.ErrRuntimeCommandState
		}
		return server.joinConnectionStart(ctx, ownerID, environmentID, *detail.ActiveOperationID)
	}
	if err != nil {
		return nil, err
	}
	operationID := command.Operation().Snapshot().ID
	return &operationID, nil
}

func (server *server) joinConnectionStart(ctx context.Context, ownerID, environmentID, operationID string) (*string, error) {
	if server.operationReads == nil {
		return nil, errors.New("Connection Intent Operation reads are unavailable")
	}
	detail, err := server.operationReads.GetOwnedOperation(ctx, ownerID, operationID)
	if err != nil {
		return nil, err
	}
	snapshot := detail.Operation.Snapshot()
	if snapshot.EnvironmentID != environmentID || snapshot.Type != domain.OperationRuntimeStart {
		return nil, db.ErrOperationConflict
	}
	return &operationID, nil
}

func connectionStartIdempotencyKey(ownerID, environmentID, clientKey string, expiresAt time.Time) string {
	digest := sha256.Sum256([]byte(ownerID + "\x00" + environmentID + "\x00" + clientKey + "\x00" + expiresAt.UTC().Format(time.RFC3339Nano)))
	return domain.SystemIdempotencyKeyPrefix + "connection-start:" + hex.EncodeToString(digest[:])
}

// ValidateRegionalProxyURLs checks the syntax of configured regional WSS
// endpoints. The control-plane binary separately requires coverage for every
// region it enables for Environment creation.
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
