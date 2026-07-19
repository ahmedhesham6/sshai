package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func (server *server) StartEnvironmentRuntime(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID, params contracts.StartEnvironmentRuntimeParams) {
	server.handleRuntimeCommand(response, request, string(environmentID), params.IdempotencyKey, true)
}

func (server *server) StopEnvironmentRuntime(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID, params contracts.StopEnvironmentRuntimeParams) {
	if err := decodeOptionalStopBody(request); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	server.handleRuntimeCommand(response, request, string(environmentID), params.IdempotencyKey, false)
}

func (server *server) handleRuntimeCommand(response http.ResponseWriter, request *http.Request, environmentID, idempotencyKey string, start bool) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	detail, err := server.environmentReads.GetOwnedEnvironment(request.Context(), user.ID, environmentID)
	if err != nil {
		server.writeRuntimeCommandError(response, request, err)
		return
	}
	input := application.RuntimeCommandInput{
		OwnerUserID: user.ID, EnvironmentID: environmentID, IdempotencyKey: idempotencyKey,
	}
	var command domain.EnvironmentRuntimeOperation
	if start {
		command, err = server.runtimeCommands.StartRuntime(request.Context(), input)
	} else {
		command, err = server.runtimeCommands.StopRuntime(request.Context(), input)
	}
	if err != nil {
		server.writeRuntimeCommandError(response, request, err)
		return
	}
	operation := command.Operation().Snapshot()
	if operation.Status == domain.OperationQueued || operation.Status == domain.OperationRunning {
		detail.ActiveOperationID = &operation.ID
	}
	body := environmentOperationResponse(detail, command.Operation())
	if start {
		result := contracts.StartEnvironmentRuntime202JSONResponse{
			Headers: contracts.StartEnvironmentRuntime202ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
			Body:    body,
		}
		if err := result.VisitStartEnvironmentRuntimeResponse(response); err != nil {
			writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
		}
		return
	}
	result := contracts.StopEnvironmentRuntime202JSONResponse{
		Headers: contracts.StopEnvironmentRuntime202ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    body,
	}
	if err := result.VisitStopEnvironmentRuntimeResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) UpdateAutoStopPolicy(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID, params contracts.UpdateAutoStopPolicyParams) {
	var body contracts.UpdateAutoStopPolicyJSONRequestBody
	if err := decodeRequiredJSON(request, &body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
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
	policyID := detail.Environment.Snapshot().AutoStopPolicyID
	update, err := server.autoStopPolicies.UpdateAutoStopPolicy(request.Context(), application.AutoStopPolicyUpdateInput{
		OwnerUserID: user.ID, EnvironmentID: string(environmentID), PolicyID: policyID,
		Mode: domain.AutoStopMode(body.Mode), GracePeriod: body.GracePeriodSeconds,
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		server.writeRuntimeCommandError(response, request, err)
		return
	}
	if update.Applied() {
		policy := update.Policy().Snapshot()
		detail.AutoStopMode, detail.GracePeriodSeconds = policy.Mode, policy.GracePeriodSeconds
		detail.ActiveOperationID = nil
	}
	result := contracts.UpdateAutoStopPolicy202JSONResponse{
		Headers: contracts.UpdateAutoStopPolicy202ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    environmentOperationResponse(detail, update.Operation()),
	}
	if err := result.VisitUpdateAutoStopPolicyResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) writeRuntimeCommandError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRuntimeCommand), errors.Is(err, application.ErrInvalidAutoStopPolicyUpdate):
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The command input is invalid.")
	case errors.Is(err, application.ErrCreditsPolicyBlocked):
		writeError(response, request, http.StatusForbidden, "CREDITS_POLICY_BLOCKED", "The Runtime cannot start while the credit balance is depleted.")
	case errors.Is(err, db.ErrReferenceNotOwned):
		writeError(response, request, http.StatusNotFound, "ENVIRONMENT_NOT_FOUND", "The Environment was not found.")
	case errors.Is(err, db.ErrIdempotencyConflict):
		writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
	case errors.Is(err, db.ErrOperationConflict):
		writeError(response, request, http.StatusConflict, "OPERATION_CONFLICT", "The Environment already has an active Operation.")
	case errors.Is(err, domain.ErrRuntimeCommandState):
		writeError(response, request, http.StatusUnprocessableEntity, "RUNTIME_COMMAND_INVALID_STATE", "The Runtime command cannot be applied to its current state.")
	default:
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The command could not be accepted safely.")
	}
}

func environmentOperationResponse(detail db.EnvironmentDetail, operation domain.Operation) contracts.EnvironmentOperation {
	snapshot := operation.Snapshot()
	return contracts.EnvironmentOperation{
		Environment: environmentResponse(detail),
		Operation: contracts.Operation{
			Id: snapshot.ID, EnvironmentId: snapshot.EnvironmentID, Type: string(snapshot.Type),
			Status: contracts.OperationStatus(snapshot.Status), Steps: []contracts.OperationStep{},
			CreatedAt: snapshot.CreatedAt, CompletedAt: snapshot.CompletedAt,
		},
	}
}

func decodeOptionalStopBody(request *http.Request) error {
	var body contracts.StopEnvironmentRuntimeJSONRequestBody
	decoder := json.NewDecoder(request.Body)
	err := decoder.Decode(&body)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	if body.Reason != nil && *body.Reason != contracts.StopEnvironmentRuntimeJSONBodyReasonManual {
		return errors.New("unsupported stop reason")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body contains multiple JSON values")
	}
	return nil
}

func decodeRequiredJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body contains multiple JSON values")
	}
	return nil
}
