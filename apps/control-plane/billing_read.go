package controlplane

import (
	"net/http"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

// GetBillingSummary reports the authenticated User's credit balance and, if
// one has ever been observed, subscription status. The credit balance is
// always present (every User has a zero-initialized balance from creation),
// so this handler cannot 404 the way owner-scoped resource Gets can.
func (server *server) GetBillingSummary(response http.ResponseWriter, request *http.Request) {
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	balance, err := server.billingReads.CreditBalance(request.Context(), user.ID)
	if err != nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The billing summary could not be loaded safely.")
		return
	}
	subscription, present, err := server.billingReads.Subscription(request.Context(), user.ID)
	if err != nil {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The billing summary could not be loaded safely.")
		return
	}
	body := contracts.BillingSummary{CreditBalance: balance.Credits, SubscriptionStatus: "none"}
	if present {
		body.SubscriptionStatus = subscription.Status
		currentPeriodEnd := subscription.CurrentPeriodEnd
		body.CurrentPeriodEnd = &currentPeriodEnd
	}
	result := contracts.GetBillingSummary200JSONResponse{
		Headers: contracts.GetBillingSummary200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    body,
	}
	if err := result.VisitGetBillingSummaryResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}
