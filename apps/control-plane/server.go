package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
)

type TokenVerifier interface {
	Verify(context.Context, string) (auth.Subject, error)
}

type UserProjection interface {
	EnsureUser(context.Context, db.EnsureUserInput) (domain.User, error)
}

type CapsulePresigner interface {
	PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignPutObject(context.Context, *s3.PutObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type CapsuleOwnership interface {
	OwnsCapsule(context.Context, string, string) (bool, error)
}

// CapsuleObjectOwnership optionally verifies non-index OCI objects before a
// pull grant is minted. Index ownership remains available through the
// CapsuleOwnership seam for callers that only need Capsule-level checks.
type CapsuleObjectOwnership interface {
	OwnsObject(context.Context, string, contracts.CapsuleAccessObjectKind, string) (bool, error)
}

// EnvironmentReader serves owner-scoped Environment read models: single
// Environment lookups, full listings, and an Environment's Operation
// timeline. The listings are keyset-paginated: a nil cursor selects the
// first page, and the returned Cursor is non-nil exactly when another page
// follows.
type EnvironmentReader interface {
	GetOwnedEnvironment(context.Context, string, string) (db.EnvironmentDetail, error)
	ListOwnedEnvironments(ctx context.Context, ownerID string, cursor *db.Cursor, pageSize int) ([]db.EnvironmentDetail, *db.Cursor, error)
	ListOwnedEnvironmentEvents(ctx context.Context, ownerID, environmentID string, cursor *db.Cursor, pageSize int) ([]db.EnvironmentEvent, *db.Cursor, error)
}

// OperationReader serves owner-scoped Operation read models.
type OperationReader interface {
	GetOwnedOperation(context.Context, string, string) (db.OperationDetail, error)
}

// ProfileReader serves owner-scoped Profile read models: single Profile
// lookups, full listings, and immutable Profile Version lookups. The
// listing is keyset-paginated: a nil cursor selects the first page, and the
// returned Cursor is non-nil exactly when another page follows.
type ProfileReader interface {
	GetOwnedProfile(context.Context, string, string) (db.ProfileDetail, error)
	ListOwnedProfiles(ctx context.Context, ownerID string, cursor *db.Cursor, pageSize int) ([]db.ProfileDetail, *db.Cursor, error)
	GetOwnedProfileVersion(context.Context, string, string) (domain.ProfileVersion, error)
}

// BillingReader serves the authenticated User's billing projection: credit
// balance (always present) and subscription (present once a Polar
// subscription has been observed).
type BillingReader interface {
	CreditBalance(context.Context, string) (db.CreditBalanceProjection, error)
	Subscription(context.Context, string) (db.SubscriptionProjection, bool, error)
}

type ConnectionIntentRepository interface {
	CreateOrReplayConnectionIntent(
		context.Context,
		string, string, string,
		time.Time, time.Time,
		func(context.Context) (*string, error),
		func() string,
	) (db.ConnectionIntentRecord, error)
}

type Config struct {
	CreateEnvironment   *application.CreateEnvironmentService
	RuntimeCommands     *application.RuntimeCommandService
	AutoStopPolicies    *application.AutoStopPolicyService
	RegisterProjectSeed *application.RegisterProjectSeedService
	Profiles            *application.ProfileService
	Uploads             *application.UploadIntentService
	SSHKeys             *application.SSHKeyService
	Verifier            TokenVerifier
	Users               UserProjection
	UserIDs             application.IDGenerator
	RequestIDs          application.IDGenerator
	ConnectionIntentIDs application.IDGenerator
	ConnectionIntents   ConnectionIntentRepository
	DefaultRegion       string
	Now                 func() time.Time
	RegionalProxyURLs   map[string]string
	ConnectionIntentTTL time.Duration
	CapsulePresigner    CapsulePresigner
	CapsuleOwnership    CapsuleOwnership
	CapsuleBucket       string
	CapsuleAccessTTL    time.Duration
	EnvironmentReads    EnvironmentReader
	OperationReads      OperationReader
	ProfileReads        ProfileReader
	BillingReads        BillingReader
}

type server struct {
	contracts.Unimplemented
	createEnvironment   *application.CreateEnvironmentService
	runtimeCommands     *application.RuntimeCommandService
	autoStopPolicies    *application.AutoStopPolicyService
	registerProjectSeed *application.RegisterProjectSeedService
	profiles            *application.ProfileService
	uploads             *application.UploadIntentService
	sshKeys             *application.SSHKeyService
	capsulePresigner    CapsulePresigner
	capsuleOwnership    CapsuleOwnership
	capsuleBucket       string
	capsuleAccessTTL    time.Duration
	now                 func() time.Time
	connectionIntentIDs application.IDGenerator
	connectionIntents   ConnectionIntentRepository
	regionalProxyURLs   map[string]*url.URL
	connectionIntentTTL time.Duration
	environmentReads    EnvironmentReader
	operationReads      OperationReader
	profileReads        ProfileReader
	billingReads        BillingReader
}

func NewHandler(config Config) http.Handler {
	regionalProxyURLs, err := parseRegionalProxyURLs(config.RegionalProxyURLs)
	if err != nil {
		panic("create control-plane handler: " + err.Error())
	}
	connectionIntentTTL := config.ConnectionIntentTTL
	if connectionIntentTTL == 0 {
		connectionIntentTTL = defaultConnectionIntentTTL
	}
	if connectionIntentTTL < 0 {
		panic("create control-plane handler: Connection Intent TTL must be positive")
	}
	api := &server{
		createEnvironment: config.CreateEnvironment, registerProjectSeed: config.RegisterProjectSeed,
		runtimeCommands: config.RuntimeCommands, autoStopPolicies: config.AutoStopPolicies,
		profiles: config.Profiles, uploads: config.Uploads, sshKeys: config.SSHKeys,
		capsulePresigner: config.CapsulePresigner, capsuleOwnership: config.CapsuleOwnership,
		capsuleBucket: config.CapsuleBucket, capsuleAccessTTL: config.CapsuleAccessTTL,
		now: config.Now, connectionIntentIDs: config.ConnectionIntentIDs, connectionIntents: config.ConnectionIntents,
		regionalProxyURLs: regionalProxyURLs, connectionIntentTTL: connectionIntentTTL,
		environmentReads: config.EnvironmentReads, operationReads: config.OperationReads,
		profileReads: config.ProfileReads, billingReads: config.BillingReads,
	}
	router := chi.NewRouter()
	router.Use(requestIDMiddleware(config.RequestIDs))
	router.Use(authenticationMiddleware(config.Verifier, config.Users, config.UserIDs, config.DefaultRegion, config.Now))
	router.Use(rejectReservedSystemIdempotencyKeys)
	specification, err := contracts.GetSwagger()
	if err != nil {
		panic("load embedded OpenAPI contract: " + err.Error())
	}
	router.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(specification, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: func(ctx context.Context, _ *openapi3filter.AuthenticationInput) error {
				if _, present := userFromContext(ctx); !present {
					return errors.New("authenticated User is missing")
				}
				return nil
			},
		},
		ErrorHandlerWithOpts: func(_ context.Context, _ error, response http.ResponseWriter, request *http.Request, options nethttpmiddleware.ErrorHandlerOpts) {
			writeError(response, request, options.StatusCode, "INVALID_REQUEST", "The request does not match the API contract.")
		},
		SilenceServersWarning: true,
	}))
	return contracts.HandlerFromMuxWithBaseURL(api, router, "/v1")
}

func rejectReservedSystemIdempotencyKeys(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.Header.Get("Idempotency-Key"), domain.SystemIdempotencyKeyPrefix) {
			writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The idempotency key uses a reserved system namespace.")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (server *server) CreateProjectSeed(response http.ResponseWriter, request *http.Request, params contracts.CreateProjectSeedParams) {
	var body contracts.CreateProjectSeedJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	seed, err := server.registerProjectSeed.RegisterProjectSeed(request.Context(), application.RegisterProjectSeedInput{
		OwnerUserID: user.ID, IdempotencyKey: params.IdempotencyKey,
		RepositoryURL: body.RepositoryUrl, BaseRevision: body.BaseRevision, Digest: body.Digest,
		GitBundleDigest:       valueOrEmpty(body.GitBundleDigest),
		TrackedPatchDigest:    valueOrEmpty(body.TrackedPatchDigest),
		UntrackedBundleDigest: valueOrEmpty(body.UntrackedBundleDigest), ManifestDigest: body.ManifestDigest,
	})
	if err != nil {
		switch {
		case errors.Is(err, application.ErrProjectSeedTransportLimit):
			writeError(response, request, http.StatusRequestEntityTooLarge, "PROJECT_SEED_TOO_LARGE", fmt.Sprintf("The Project Seed uploads exceed the %d MiB guest transport limit.", application.ProjectSeedTransportMaximumRawBytes>>20))
		case errors.Is(err, application.ErrUploadNotVerified):
			writeError(response, request, http.StatusBadRequest, "INVALID_UPLOAD", "A Project Seed upload is not valid.")
		case errors.Is(err, application.ErrUploadObjectNotFound), errors.Is(err, db.ErrReferenceNotOwned):
			writeError(response, request, http.StatusNotFound, "UPLOAD_NOT_FOUND", "A Project Seed upload was not found.")
		case errors.Is(err, application.ErrInvalidProjectSeed):
			writeError(response, request, http.StatusBadRequest, "INVALID_PROJECT_SEED", "The Project Seed metadata is invalid.")
		case errors.Is(err, db.ErrIdempotencyConflict):
			writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
		default:
			writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The Project Seed could not be registered safely.")
		}
		return
	}
	snapshot := seed.Snapshot()
	result := contracts.CreateProjectSeed201JSONResponse{
		Headers: contracts.CreateProjectSeed201ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
	}
	result.Body.Id, result.Body.Digest = snapshot.ID, snapshot.Digest
	if err := result.VisitCreateProjectSeedResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (server *server) CreateEnvironment(response http.ResponseWriter, request *http.Request, params contracts.CreateEnvironmentParams) {
	var body contracts.CreateEnvironmentJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	creation, err := server.createEnvironment.CreateEnvironment(request.Context(), application.CreateEnvironmentInput{
		OwnerUserID: user.ID, Name: body.Name, Region: body.Region, RuntimePreset: body.RuntimePreset,
		ProfileVersionID: body.ProfileVersionId, ProjectSeedID: body.ProjectSeedId,
		SSHKeyIDs: body.SshKeyIds, AutoStopMode: domain.AutoStopMode(body.AutoStopPolicy.Mode),
		GracePeriod: body.AutoStopPolicy.GracePeriodSeconds, IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		server.writeCreateError(response, request, err)
		return
	}
	environment, operation, policy := creation.Environment().Snapshot(), creation.Operation().Snapshot(), creation.Policy().Snapshot()
	activeOperationID := operation.ID
	result := contracts.CreateEnvironment202JSONResponse{
		Headers: contracts.CreateEnvironment202ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body: contracts.EnvironmentOperation{
			Environment: contracts.Environment{
				Id: environment.ID, Name: environment.Name, Slug: environment.Slug,
				Lifecycle: contracts.EnvironmentLifecycle(environment.Lifecycle), Health: contracts.EnvironmentHealth(environment.Health),
				Region: environment.Region, RuntimePreset: environment.RuntimePreset,
				PinnedProfileVersionId: environment.PinnedProfileVersionID,
				AutoStopPolicy: contracts.AutoStopPolicy{
					Mode: contracts.AutoStopPolicyMode(policy.Mode), GracePeriodSeconds: policy.GracePeriodSeconds,
				},
				ActiveOperationId: &activeOperationID, CreatedAt: environment.CreatedAt,
			},
			Operation: contracts.Operation{
				Id: operation.ID, EnvironmentId: operation.EnvironmentID, Type: string(operation.Type),
				Status: contracts.OperationStatus(operation.Status), Steps: []contracts.OperationStep{}, CreatedAt: operation.CreatedAt,
			},
		},
	}
	if err := result.VisitCreateEnvironmentResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) writeCreateError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrRegionUnavailable):
		writeError(response, request, http.StatusUnprocessableEntity, "REGION_UNAVAILABLE", "The selected region is unavailable.")
	case errors.Is(err, db.ErrIdempotencyConflict):
		writeError(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "The idempotency key was already used with different input.")
	case errors.Is(err, db.ErrReferenceNotOwned):
		writeError(response, request, http.StatusNotFound, "REFERENCE_NOT_FOUND", "A referenced resource was not found.")
	default:
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "The command could not be accepted safely.")
	}
}

type contextKey string

const (
	requestIDKey contextKey = "request-id"
	userKey      contextKey = "user"
)

func requestIDMiddleware(ids application.IDGenerator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			requestID := ids.NewID()
			response.Header().Set("X-Request-ID", requestID)
			next.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), requestIDKey, requestID)))
		})
	}
}

func authenticationMiddleware(verifier TokenVerifier, users UserProjection, ids application.IDGenerator, defaultRegion string, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			token, present := bearerToken(request.Header.Get("Authorization"))
			if !present {
				writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "A valid bearer token is required.")
				return
			}
			subject, err := verifier.Verify(request.Context(), token)
			if err != nil {
				writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "A valid bearer token is required.")
				return
			}
			user, err := users.EnsureUser(request.Context(), db.EnsureUserInput{
				ID: ids.NewID(), WorkOSUserID: subject.WorkOSUserID, DefaultRegion: defaultRegion, ObservedAt: now(),
			})
			if err != nil {
				writeError(response, request, http.StatusServiceUnavailable, "AUTHENTICATION_UNAVAILABLE", "Authentication could not be completed.")
				return
			}
			next.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), userKey, user)))
		})
	}
}

func bearerToken(authorization string) (string, bool) {
	scheme, token, present := strings.Cut(strings.TrimSpace(authorization), " ")
	return token, present && strings.EqualFold(scheme, "Bearer") && token != ""
}

func writeError(response http.ResponseWriter, request *http.Request, status int, code, message string) {
	writeErrorWithOperation(response, request, status, code, message, nil)
}

func writeErrorWithOperation(response http.ResponseWriter, request *http.Request, status int, code, message string, operationID *string) {
	body := contracts.ErrorResponse{RequestId: requestIDFromContext(request.Context())}
	body.Error.Code, body.Error.Message = code, message
	body.Error.OperationId = operationID
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Request-ID", body.RequestId)
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)
	return requestID
}

func userFromContext(ctx context.Context) (domain.User, bool) {
	user, present := ctx.Value(userKey).(domain.User)
	return user, present
}
