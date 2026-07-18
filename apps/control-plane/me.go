package controlplane

import (
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

// GetCurrentUser reports the authenticated User already resolved by the
// authentication middleware; it requires no additional store lookup.
func (server *server) GetCurrentUser(response http.ResponseWriter, request *http.Request) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	result := contracts.GetCurrentUser200JSONResponse{
		Headers: contracts.GetCurrentUser200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    contracts.User{Id: user.ID, DefaultRegion: user.DefaultRegion},
	}
	if err := result.VisitGetCurrentUserResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}
