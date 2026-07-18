package controlplane

import (
	"errors"
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

func (server *server) ListEnvironments(response http.ResponseWriter, request *http.Request, params contracts.ListEnvironmentsParams) {
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
	details, nextCursor, err := server.environmentReads.ListOwnedEnvironments(request.Context(), user.ID, cursor, pageSize)
	if err != nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Environments could not be listed safely.")
		return
	}
	items := make([]contracts.Environment, len(details))
	for index, detail := range details {
		items[index] = environmentResponse(detail)
	}
	page := contracts.EnvironmentPage{Items: items}
	if nextCursor != nil {
		encoded := db.EncodeCursor(*nextCursor)
		page.NextCursor = &encoded
	}
	result := contracts.ListEnvironments200JSONResponse{
		Headers: contracts.ListEnvironments200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    page,
	}
	if err := result.VisitListEnvironmentsResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) GetEnvironment(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	detail, err := server.environmentReads.GetOwnedEnvironment(request.Context(), user.ID, string(environmentID))
	if err != nil {
		if errors.Is(err, db.ErrReferenceNotOwned) {
			writeError(response, request, http.StatusNotFound, "ENVIRONMENT_NOT_FOUND", "The Environment was not found.")
			return
		}
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Environment could not be loaded safely.")
		return
	}
	result := contracts.GetEnvironment200JSONResponse{
		Headers: contracts.GetEnvironment200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    environmentResponse(detail),
	}
	if err := result.VisitGetEnvironmentResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) ListEnvironmentEvents(response http.ResponseWriter, request *http.Request, environmentID contracts.EnvironmentID, params contracts.ListEnvironmentEventsParams) {
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
	events, nextCursor, err := server.environmentReads.ListOwnedEnvironmentEvents(request.Context(), user.ID, string(environmentID), cursor, pageSize)
	if err != nil {
		if errors.Is(err, db.ErrReferenceNotOwned) {
			writeError(response, request, http.StatusNotFound, "ENVIRONMENT_NOT_FOUND", "The Environment was not found.")
			return
		}
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Environment events could not be listed safely.")
		return
	}
	items := make([]contracts.EnvironmentEvent, len(events))
	for index, event := range events {
		items[index] = contracts.EnvironmentEvent{
			Id: event.ID, EnvironmentId: event.EnvironmentID, OperationId: event.OperationID,
			Type: event.Type, Summary: event.Summary, CreatedAt: event.CreatedAt,
		}
	}
	page := contracts.EnvironmentEventPage{Items: items}
	if nextCursor != nil {
		encoded := db.EncodeCursor(*nextCursor)
		page.NextCursor = &encoded
	}
	result := contracts.ListEnvironmentEvents200JSONResponse{
		Headers: contracts.ListEnvironmentEvents200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    page,
	}
	if err := result.VisitListEnvironmentEventsResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

// environmentResponse maps an owner-scoped Environment read projection onto
// the public contract. capsuleLockId is required and non-nullable by the
// OpenAPI contract, but an Environment has none until its first Profile
// resolve completes; until then this reports an empty string rather than
// fabricating an identifier.
func environmentResponse(detail db.EnvironmentDetail) contracts.Environment {
	snapshot := detail.Environment.Snapshot()
	capsuleLockID := ""
	if snapshot.CapsuleLockID != nil {
		capsuleLockID = *snapshot.CapsuleLockID
	}
	body := contracts.Environment{
		Id: snapshot.ID, Name: snapshot.Name, Slug: snapshot.Slug,
		Lifecycle: contracts.EnvironmentLifecycle(snapshot.Lifecycle), Health: contracts.EnvironmentHealth(snapshot.Health),
		Region: snapshot.Region, RuntimePreset: snapshot.RuntimePreset,
		PinnedProfileVersionId: snapshot.PinnedProfileVersionID, CapsuleLockId: capsuleLockID,
		AutoStopPolicy: contracts.AutoStopPolicy{
			Mode: contracts.AutoStopPolicyMode(detail.AutoStopMode), GracePeriodSeconds: detail.GracePeriodSeconds,
		},
		ActiveOperationId: detail.ActiveOperationID, CreatedAt: snapshot.CreatedAt,
	}
	if detail.Runtime != nil {
		runtimeSnapshot := detail.Runtime.Snapshot()
		body.Runtime = &contracts.Runtime{
			Id: runtimeSnapshot.ID, Status: contracts.RuntimeStatus(runtimeSnapshot.Status),
			RuntimePreset: runtimeSnapshot.RuntimePreset, Region: runtimeSnapshot.Region,
			AvailabilityZone: runtimeSnapshot.AvailabilityZone, ImageVersion: runtimeSnapshot.ImageVersion,
		}
	}
	if detail.CapsuleLock != nil {
		lockSnapshot := detail.CapsuleLock.Snapshot()
		capsules := make([]contracts.LockedCapsule, len(lockSnapshot.Capsules))
		for index, capsule := range lockSnapshot.Capsules {
			capsules[index] = contracts.LockedCapsule{Ref: capsule.Ref, Digest: capsule.Digest}
		}
		body.CapsuleLock = &contracts.CapsuleLock{
			Id: lockSnapshot.ID, Digest: lockSnapshot.Digest, ProfileVersionId: lockSnapshot.ProfileVersionID,
			ProjectCapsuleDigest: lockSnapshot.ProjectCapsuleDigest, Capsules: capsules,
		}
	}
	return body
}
