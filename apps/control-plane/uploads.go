package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func (server *server) CreateUploadIntent(response http.ResponseWriter, request *http.Request, params contracts.CreateUploadIntentParams) {
	var body contracts.CreateUploadIntentJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	result, err := server.uploads.Create(request.Context(), application.CreateUploadIntentInput{
		OwnerUserID: user.ID, IdempotencyKey: params.IdempotencyKey, Kind: domain.UploadKind(body.Kind),
		Digest: body.Digest, SizeBytes: body.SizeBytes,
	})
	if err != nil {
		switch {
		case errors.Is(err, application.ErrInvalidUploadIntent):
			writeError(response, request, http.StatusBadRequest, "INVALID_UPLOAD", "The upload metadata is invalid.")
		case errors.Is(err, db.ErrIdempotencyConflict):
			writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
		default:
			writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The upload could not be prepared safely.")
		}
		return
	}
	snapshot := result.Intent.Snapshot()
	created := contracts.CreateUploadIntent201JSONResponse{
		Headers: contracts.CreateUploadIntent201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
	}
	created.Body.UploadId = snapshot.ID
	created.Body.Url = result.URL
	created.Body.ExpiresAt = snapshot.ExpiresAt
	created.Body.RequiredHeaders = result.RequiredHeaders
	if err := created.VisitCreateUploadIntentResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}
