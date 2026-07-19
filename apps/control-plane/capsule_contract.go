package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var (
	capsuleOwnerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	capsuleDigestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

var ErrCapsuleRefNotOwned = errors.New("Capsule Ref is not owned by the authenticated User")

type CapsuleObjectInspector interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

type S3CapsuleOwnership struct {
	client CapsuleObjectInspector
	bucket string
}

func NewS3CapsuleOwnership(client CapsuleObjectInspector, bucket string) *S3CapsuleOwnership {
	return &S3CapsuleOwnership{client: client, bucket: bucket}
}

func (ownership *S3CapsuleOwnership) OwnsCapsule(ctx context.Context, ownerID, digest string) (bool, error) {
	return ownership.OwnsObject(ctx, ownerID, contracts.Index, digest)
}

func (ownership *S3CapsuleOwnership) OwnsObject(ctx context.Context, ownerID string, kind contracts.CapsuleAccessObjectKind, digest string) (bool, error) {
	if ownership == nil || ownership.client == nil || strings.TrimSpace(ownership.bucket) == "" {
		return false, errors.New("Capsule ownership inspector is not configured")
	}
	key, err := capsuleObjectKey(ownerID, kind, digest)
	if err != nil {
		return false, err
	}
	_, err = ownership.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(ownership.bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && (apiError.ErrorCode() == "NoSuchKey" || apiError.ErrorCode() == "NotFound") {
		return false, nil
	}
	return false, fmt.Errorf("inspect Capsule ownership: %w", err)
}

// ValidatePublishProfileVersionRequest validates the domain-specific parts of
// the generated Profile Version publication request.
func ValidatePublishProfileVersionRequest(body contracts.PublishProfileVersionJSONRequestBody) error {
	for index, capsuleRef := range body.CapsuleRefs {
		if !capsuleRef.FreshnessPolicy.Valid() {
			return fmt.Errorf("capsuleRefs[%d].freshnessPolicy is unsupported", index)
		}
		if _, err := contracts.ParseOwnedCapsuleRef(capsuleRef.Ref); err != nil {
			return fmt.Errorf("capsuleRefs[%d].ref is invalid: %w", index, err)
		}
	}
	return nil
}

func ValidatePublishProfileVersionRequestForOwner(body contracts.PublishProfileVersionJSONRequestBody, ownerID string) error {
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("authenticated owner ID is required")
	}
	if err := ValidatePublishProfileVersionRequest(body); err != nil {
		return err
	}
	for index, capsuleRef := range body.CapsuleRefs {
		parsed, err := contracts.ParseOwnedCapsuleRef(capsuleRef.Ref)
		if err != nil {
			return fmt.Errorf("capsuleRefs[%d].ref is invalid: %w", index, err)
		}
		if parsed.OwnerID != ownerID {
			return fmt.Errorf("capsuleRefs[%d].ref: %w", index, ErrCapsuleRefNotOwned)
		}
	}
	return nil
}

// ValidateCapsuleAccessRequest validates the generated Capsule access request
// before grant minting reaches S3.
func ValidateCapsuleAccessRequest(body contracts.CreateCapsuleAccessJSONRequestBody) error {
	if !body.Intent.Valid() {
		return fmt.Errorf("intent must be pull or push")
	}
	if len(body.Objects) == 0 {
		return fmt.Errorf("objects must contain at least one OCI object")
	}
	for index, object := range body.Objects {
		if !object.Kind.Valid() {
			return fmt.Errorf("objects[%d].kind is unsupported", index)
		}
		if !capsuleDigestPattern.MatchString(object.Digest) {
			return fmt.Errorf("objects[%d].digest is not a valid sha256 digest", index)
		}
	}
	return nil
}

func (server *server) CreateCapsuleAccess(response http.ResponseWriter, request *http.Request) {
	var body contracts.CreateCapsuleAccessJSONRequestBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", "The request body is not valid JSON.")
		return
	}
	if err := ValidateCapsuleAccessRequest(body); err != nil {
		writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	user, present := userFromContext(request.Context())
	if !present {
		writeError(response, request, http.StatusUnauthorized, "AUTHORIZATION_FAILED", "Authentication is required.")
		return
	}
	if server.capsulePresigner == nil || strings.TrimSpace(server.capsuleBucket) == "" {
		writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Capsule access grants could not be minted safely.")
		return
	}
	ttl := server.capsuleAccessTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	now := time.Now().UTC()
	if server.now != nil {
		now = server.now().UTC()
	}
	grants := make([]contracts.CapsuleAccessGrant, 0, len(body.Objects))
	for _, object := range body.Objects {
		key, err := capsuleObjectKey(user.ID, object.Kind, object.Digest)
		if err != nil {
			writeError(response, request, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if body.Intent == contracts.Pull {
			owned, ownershipErr := server.ownsCapsuleObject(request.Context(), user.ID, object)
			if ownershipErr != nil {
				writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Capsule ownership could not be verified safely.")
				return
			}
			if !owned {
				continue
			}
		}
		grant, err := server.presignCapsuleObject(request.Context(), body.Intent, key, ttl)
		if err != nil {
			writeError(response, request, http.StatusServiceUnavailable, "COMMAND_UNAVAILABLE", "Capsule access grants could not be minted safely.")
			return
		}
		grant.ExpiresAt = now.Add(ttl)
		grants = append(grants, grant)
	}
	result := contracts.CreateCapsuleAccess200JSONResponse{
		Headers: contracts.CreateCapsuleAccess200ResponseHeaders{XRequestID: requestIDFromContext(request.Context())},
		Body:    contracts.CapsuleAccessResponse{Grants: grants},
	}
	if err := result.VisitCreateCapsuleAccessResponse(response); err != nil {
		writeError(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "The response could not be encoded.")
	}
}

func (server *server) ownsCapsuleObject(ctx context.Context, ownerID string, object contracts.CapsuleAccessObject) (bool, error) {
	if server.capsuleOwnership == nil {
		return false, nil
	}
	if object.Kind == contracts.Index {
		return server.capsuleOwnership.OwnsCapsule(ctx, ownerID, object.Digest)
	}
	if ownership, ok := server.capsuleOwnership.(CapsuleObjectOwnership); ok {
		return ownership.OwnsObject(ctx, ownerID, object.Kind, object.Digest)
	}
	// The generated key is always derived from the authenticated owner. A
	// legacy CapsuleOwnership implementation can therefore still safely mint a
	// non-index grant without broadening the owner prefix.
	return true, nil
}

func (server *server) presignCapsuleObject(ctx context.Context, intent contracts.CapsuleAccessRequestIntent, key string, ttl time.Duration) (contracts.CapsuleAccessGrant, error) {
	if !strings.HasPrefix(key, "owner/") || strings.ContainsAny(key, "\r\n") {
		return contracts.CapsuleAccessGrant{}, errors.New("Capsule object key is outside the owner prefix")
	}
	var signed *v4.PresignedHTTPRequest
	var err error
	method := contracts.GET
	if intent == contracts.Pull {
		signed, err = server.capsulePresigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(server.capsuleBucket), Key: aws.String(key)}, s3.WithPresignExpires(ttl))
	} else {
		method = contracts.PUT
		signed, err = server.capsulePresigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(server.capsuleBucket), Key: aws.String(key), IfNoneMatch: aws.String("*")}, s3.WithPresignExpires(ttl))
	}
	if err != nil {
		return contracts.CapsuleAccessGrant{}, fmt.Errorf("presign Capsule object: %w", err)
	}
	if signed == nil {
		return contracts.CapsuleAccessGrant{}, errors.New("presigner returned no request")
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return contracts.CapsuleAccessGrant{}, errors.New("presigner returned an invalid URL")
	}
	headers := make(map[string]string, len(signed.SignedHeader))
	for name, values := range signed.SignedHeader {
		if len(values) != 1 || strings.ContainsAny(name+values[0], "\r\n") {
			return contracts.CapsuleAccessGrant{}, errors.New("presigner returned ambiguous headers")
		}
		headers[name] = values[0]
	}
	if intent == contracts.Push && headers["If-None-Match"] != "*" {
		return contracts.CapsuleAccessGrant{}, errors.New("presigner did not sign the required If-None-Match header")
	}
	return contracts.CapsuleAccessGrant{Url: parsed.String(), Method: method, Headers: headers}, nil
}

func capsuleObjectKey(ownerID string, kind contracts.CapsuleAccessObjectKind, digest string) (string, error) {
	if !capsuleOwnerIDPattern.MatchString(ownerID) {
		return "", errors.New("Capsule owner ID is invalid")
	}
	var key string
	switch kind {
	case contracts.Index:
		key = oci.IndexKey(ownerID, digest)
	case contracts.Manifest:
		key = oci.ManifestKey(ownerID, digest)
	case contracts.Blob:
		key = oci.BlobKey(ownerID, digest)
	default:
		return "", fmt.Errorf("unsupported Capsule object kind %q", kind)
	}
	if key == "" || !strings.HasPrefix(key, "owner/") || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("Capsule object key is invalid")
	}
	return key, nil
}
