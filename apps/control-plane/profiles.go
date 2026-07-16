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

func (server *server) CreateProfile(response http.ResponseWriter, request *http.Request, params contracts.CreateProfileParams) {
	var body contracts.CreateProfileJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	profile, err := server.profiles.CreateProfile(request.Context(), application.CreateProfileInput{
		OwnerUserID: user.ID, Name: body.Name, ForkedFromVersionID: body.ForkedFromVersionId,
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		writeProfileError(response, request, err)
		return
	}
	snapshot := profile.Snapshot()
	result := contracts.CreateProfile201JSONResponse{
		Headers: contracts.CreateProfile201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    contracts.ProfileSummary{Id: snapshot.ID, Name: snapshot.Name, Slug: snapshot.Slug},
	}
	if err := result.VisitCreateProfileResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) PublishProfileVersion(response http.ResponseWriter, request *http.Request, profileID contracts.ProfileID, params contracts.PublishProfileVersionParams) {
	var body contracts.PublishProfileVersionJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	if err := ValidatePublishProfileVersionRequest(body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	if err := ValidatePublishProfileVersionRequestForOwner(body, user.ID); err != nil {
		if errors.Is(err, ErrCapsuleRefNotOwned) {
			writeError(response, request, http.StatusUnprocessableEntity, "PROFILE_INCOMPATIBLE", "Every Capsule Ref must belong to the authenticated User.")
			return
		}
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	refs := make([]domain.CapsuleRef, len(body.CapsuleRefs))
	for index, ref := range body.CapsuleRefs {
		var exclusions []string
		if ref.Exclusions != nil {
			exclusions = append([]string(nil), (*ref.Exclusions)...)
		}
		refs[index] = domain.CapsuleRef{
			Ref: ref.Ref, FreshnessPolicy: domain.FreshnessPolicy(ref.FreshnessPolicy), Exclusions: exclusions,
		}
	}
	version, err := server.profiles.PublishProfileVersion(request.Context(), application.PublishProfileVersionInput{
		OwnerUserID: user.ID, ProfileID: string(profileID), ExpectedHeadVersionID: body.ExpectedHeadVersionId,
		CapsuleRefs: refs, IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		writeProfileError(response, request, err)
		return
	}
	snapshot := version.Snapshot()
	result := contracts.PublishProfileVersion201JSONResponse{
		Headers: contracts.PublishProfileVersion201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body: contracts.ProfileVersion{
			Id: snapshot.ID, ProfileId: snapshot.ProfileID, ParentVersionId: snapshot.ParentVersionID,
			Version: snapshot.Version, Digest: snapshot.Digest, CreatedAt: snapshot.CreatedAt,
			CapsuleRefs: capsuleRefsResponse(snapshot.CapsuleRefs),
		},
	}
	if err := result.VisitPublishProfileVersionResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func capsuleRefsResponse(refs []domain.CapsuleRef) []contracts.CapsuleRef {
	result := make([]contracts.CapsuleRef, len(refs))
	for index, ref := range refs {
		exclusions := append([]string(nil), ref.Exclusions...)
		result[index] = contracts.CapsuleRef{Ref: ref.Ref, FreshnessPolicy: contracts.CapsuleRefFreshnessPolicy(ref.FreshnessPolicy)}
		if exclusions != nil {
			result[index].Exclusions = &exclusions
		}
	}
	return result
}

func writeProfileError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrProfileForkUnsupported):
		writeError(response, request, http.StatusUnprocessableEntity, "PROFILE_FORK_UNSUPPORTED", "Profile forks are not supported yet.")
	case errors.Is(err, application.ErrInvalidProfileCommand), errors.Is(err, db.ErrInvalidProfilePublication):
		writeError(response, request, http.StatusBadRequest, "INVALID_PROFILE", "The Profile command is invalid.")
	case errors.Is(err, db.ErrIdempotencyConflict):
		writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
	case errors.Is(err, db.ErrProfileConflict):
		writeError(response, request, http.StatusConflict, "PROFILE_CONFLICT", "An active Profile with this name already exists.")
	case errors.Is(err, domain.ErrStaleProfileHead):
		writeError(response, request, http.StatusConflict, "STALE_PROFILE_HEAD", "The Profile head changed; refresh it before publishing.")
	case errors.Is(err, db.ErrReferenceNotOwned):
		writeError(response, request, http.StatusNotFound, "PROFILE_NOT_FOUND", "The Profile was not found.")
	default:
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Profile command could not be accepted safely.")
	}
}
