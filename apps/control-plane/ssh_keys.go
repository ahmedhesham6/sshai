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

func (server *server) CreateSSHKey(response http.ResponseWriter, request *http.Request, params contracts.CreateSSHKeyParams) {
	var body contracts.CreateSSHKeyJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	key, err := server.sshKeys.Register(request.Context(), application.RegisterSSHKeyInput{
		OwnerUserID: user.ID, IdempotencyKey: params.IdempotencyKey, Label: body.Label, PublicKey: body.PublicKey,
	})
	if err != nil {
		writeSSHKeyError(response, request, err)
		return
	}
	result := contracts.CreateSSHKey201JSONResponse{
		Headers: contracts.CreateSSHKey201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    publicSSHKey(key),
	}
	if err := result.VisitCreateSSHKeyResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) ListSSHKeys(response http.ResponseWriter, request *http.Request, _ contracts.ListSSHKeysParams) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	keys, err := server.sshKeys.List(request.Context(), user.ID)
	if err != nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "SSH Keys could not be listed safely.")
		return
	}
	items := make([]contracts.SSHKey, len(keys))
	for index, key := range keys {
		items[index] = publicSSHKey(key)
	}
	result := contracts.ListSSHKeys200JSONResponse{
		Headers: contracts.ListSSHKeys200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    contracts.SSHKeyPage{Items: items},
	}
	if err := result.VisitListSSHKeysResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) RevokeSSHKey(response http.ResponseWriter, request *http.Request, keyID contracts.KeyID, params contracts.RevokeSSHKeyParams) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	err := server.sshKeys.Revoke(request.Context(), application.RevokeSSHKeyInput{
		OwnerUserID: user.ID, SSHKeyID: keyID, IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		writeSSHKeyError(response, request, err)
		return
	}
	result := contracts.RevokeSSHKey204Response{Headers: contracts.RevokeSSHKey204ResponseHeaders{XRequestID: requestIDFromContext(request.Context())}}
	if err := result.VisitRevokeSSHKeyResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func publicSSHKey(key domain.SSHKey) contracts.SSHKey {
	snapshot := key.Snapshot()
	return contracts.SSHKey{
		Id: snapshot.ID, Label: snapshot.Label, Algorithm: contracts.SSHKeyAlgorithm(snapshot.Algorithm),
		Fingerprint: snapshot.Fingerprint, PublicKey: snapshot.PublicKey, CreatedAt: snapshot.CreatedAt,
	}
}

func writeSSHKeyError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidSSHKey):
		writeError(response, request, http.StatusBadRequest, "INVALID_SSH_KEY", "The SSH public key is invalid.")
	case errors.Is(err, db.ErrIdempotencyConflict):
		writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
	case errors.Is(err, db.ErrReferenceNotOwned):
		writeError(response, request, http.StatusNotFound, "SSH_KEY_NOT_FOUND", "The SSH Key was not found.")
	default:
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The SSH Key command could not be accepted safely.")
	}
}
