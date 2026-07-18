package controlplane

import (
	"errors"
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

func (server *server) ListProfiles(response http.ResponseWriter, request *http.Request, params contracts.ListProfilesParams) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	cursor, pageSize, ok := decodePageParams(params.Cursor, params.PageSize)
	if !ok {
		writeInvalidCursor(response, request)
		return
	}
	details, nextCursor, err := server.profileReads.ListOwnedProfiles(request.Context(), user.ID, cursor, pageSize)
	if err != nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Profiles could not be listed safely.")
		return
	}
	items := make([]contracts.ProfileSummary, len(details))
	for index, detail := range details {
		items[index] = profileSummaryResponse(detail)
	}
	page := contracts.ProfilePage{Items: items}
	if nextCursor != nil {
		encoded := db.EncodeCursor(*nextCursor)
		page.NextCursor = &encoded
	}
	result := contracts.ListProfiles200JSONResponse{
		Headers: contracts.ListProfiles200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    page,
	}
	if err := result.VisitListProfilesResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) GetProfile(response http.ResponseWriter, request *http.Request, profileID contracts.ProfileID) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	detail, err := server.profileReads.GetOwnedProfile(request.Context(), user.ID, string(profileID))
	if err != nil {
		if errors.Is(err, db.ErrReferenceNotOwned) {
			writeError(response, request, http.StatusNotFound, "PROFILE_NOT_FOUND", "The Profile was not found.")
			return
		}
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Profile could not be loaded safely.")
		return
	}
	result := contracts.GetProfile200JSONResponse{
		Headers: contracts.GetProfile200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    profileSummaryResponse(detail),
	}
	if err := result.VisitGetProfileResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) GetProfileVersion(response http.ResponseWriter, request *http.Request, versionID string) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	version, err := server.profileReads.GetOwnedProfileVersion(request.Context(), user.ID, versionID)
	if err != nil {
		if errors.Is(err, db.ErrReferenceNotOwned) {
			writeError(response, request, http.StatusNotFound, "PROFILE_VERSION_NOT_FOUND", "The Profile Version was not found.")
			return
		}
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Profile Version could not be loaded safely.")
		return
	}
	snapshot := version.Snapshot()
	result := contracts.GetProfileVersion200JSONResponse{
		Headers: contracts.GetProfileVersion200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body: contracts.ProfileVersion{
			Id: snapshot.ID, ProfileId: snapshot.ProfileID, ParentVersionId: snapshot.ParentVersionID,
			Version: snapshot.Version, Digest: snapshot.Digest, CreatedAt: snapshot.CreatedAt,
			CapsuleRefs: capsuleRefsResponse(snapshot.CapsuleRefs),
		},
	}
	if err := result.VisitGetProfileVersionResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func profileSummaryResponse(detail db.ProfileDetail) contracts.ProfileSummary {
	snapshot := detail.Profile.Snapshot()
	return contracts.ProfileSummary{Id: snapshot.ID, Name: snapshot.Name, Slug: snapshot.Slug, HeadVersionId: detail.HeadVersionID}
}
