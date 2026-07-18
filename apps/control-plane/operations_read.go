package controlplane

import (
	"errors"
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

func (server *server) GetOperation(response http.ResponseWriter, request *http.Request, operationID string) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	detail, err := server.operationReads.GetOwnedOperation(request.Context(), user.ID, operationID)
	if err != nil {
		if errors.Is(err, db.ErrReferenceNotOwned) {
			writeError(response, request, http.StatusNotFound, "OPERATION_NOT_FOUND", "The Operation was not found.")
			return
		}
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Operation could not be loaded safely.")
		return
	}
	result := contracts.GetOperation200JSONResponse{
		Headers: contracts.GetOperation200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    operationResponse(detail),
	}
	if err := result.VisitGetOperationResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func operationResponse(detail db.OperationDetail) contracts.Operation {
	snapshot := detail.Operation.Snapshot()
	steps := make([]contracts.OperationStep, len(detail.Steps))
	for index, step := range detail.Steps {
		steps[index] = contracts.OperationStep{
			StepKey: step.StepKey, Status: contracts.OperationStepStatus(step.Status), Summary: step.Summary,
		}
	}
	body := contracts.Operation{
		Id: snapshot.ID, EnvironmentId: snapshot.EnvironmentID, Type: string(snapshot.Type),
		Status: contracts.OperationStatus(snapshot.Status), Steps: steps, CreatedAt: snapshot.CreatedAt,
	}
	if snapshot.CompletedAt != nil {
		completedAt := *snapshot.CompletedAt
		body.CompletedAt = &completedAt
	}
	return body
}
