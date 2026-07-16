package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestCapsuleAccessRejectsDigestNotOwnedByAuthenticatedUser(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: false}, now)

	response := serveCapsuleAccessRequest(handler, true, `{"intent":"pull","objects":[{"kind":"index","digest":"sha256:`+strings.Repeat("a", 64)+`"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("Capsule access status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var body contracts.CapsuleAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode Capsule access response: %v", err)
	}
	if len(body.Grants) != 0 || len(presigner.requests) != 0 {
		t.Fatalf("cross-owner Capsule access grants = %#v, presigns = %#v; want none", body.Grants, presigner.requests)
	}
}

func TestCapsuleAccessSetsShortExpiryOnPullGrant(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: true}, now)

	response := serveCapsuleAccessRequest(handler, true, `{"intent":"pull","objects":[{"kind":"index","digest":"sha256:`+strings.Repeat("b", 64)+`"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("Capsule access status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var body contracts.CapsuleAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode Capsule access response: %v", err)
	}
	if len(body.Grants) != 1 {
		t.Fatalf("pull grants = %#v, want one grant", body.Grants)
	}
	if !body.Grants[0].ExpiresAt.After(now) || body.Grants[0].ExpiresAt.Sub(now) != 15*time.Minute {
		t.Fatalf("grant expiry = %s, want %s", body.Grants[0].ExpiresAt, now.Add(15*time.Minute))
	}
}

func TestCapsuleAccessMintsOneGrantForEachExplicitOCIObject(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: true}, now)
	digest := strings.Repeat("b", 64)
	body := `{"intent":"pull","objects":[` +
		`{"kind":"index","digest":"sha256:` + digest + `"},` +
		`{"kind":"manifest","digest":"sha256:` + strings.Repeat("c", 64) + `"},` +
		`{"kind":"blob","digest":"sha256:` + strings.Repeat("d", 64) + `"}]}`

	response := serveCapsuleAccessRequest(handler, true, body)
	if response.Code != http.StatusOK {
		t.Fatalf("Capsule access status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var result contracts.CapsuleAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode Capsule access response: %v", err)
	}
	if len(result.Grants) != 3 || len(presigner.requests) != 3 {
		t.Fatalf("explicit object grants = %#v, presigns = %#v; want three", result.Grants, presigner.requests)
	}
	wantKeys := []string{
		oci.IndexKey("user-1", "sha256:"+digest),
		oci.ManifestKey("user-1", "sha256:"+strings.Repeat("c", 64)),
		oci.BlobKey("user-1", "sha256:"+strings.Repeat("d", 64)),
	}
	for index, want := range wantKeys {
		if presigner.requests[index].key != want {
			t.Fatalf("presign[%d] key = %q, want %q", index, presigner.requests[index].key, want)
		}
	}
}

func TestCapsuleAccessPushGrantCannotEscapeOwnerPrefix(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: true}, now)

	response := serveCapsuleAccessRequest(handler, true, `{"intent":"push","objects":[{"kind":"index","digest":"sha256:`+strings.Repeat("c", 64)+`"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("Capsule access status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if len(presigner.requests) != 1 || presigner.requests[0].method != http.MethodPut {
		t.Fatalf("push presign requests = %#v, want one PUT", presigner.requests)
	}
	wantPrefix := "owner/user-1/"
	if !strings.HasPrefix(presigner.requests[0].key, wantPrefix) || strings.Contains(presigner.requests[0].key, "user-2") {
		t.Fatalf("push key = %q, want prefix %q and no foreign owner", presigner.requests[0].key, wantPrefix)
	}
	if presigner.requests[0].key != oci.IndexKey("user-1", "sha256:"+strings.Repeat("c", 64)) {
		t.Fatalf("push key = %q, want OCI index key", presigner.requests[0].key)
	}
}

func TestCapsuleAccessPushGrantSignsConditionalCreateHeader(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: true}, now)

	response := serveCapsuleAccessRequest(handler, true, `{"intent":"push","objects":[{"kind":"blob","digest":"sha256:`+strings.Repeat("e", 64)+`"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("conditional push status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var result contracts.CapsuleAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode conditional push response: %v", err)
	}
	if len(result.Grants) != 1 || result.Grants[0].Headers["If-None-Match"] != "*" {
		t.Fatalf("conditional push grant = %#v, want If-None-Match: *", result.Grants)
	}
	if len(presigner.requests) != 1 || presigner.requests[0].ifNoneMatch != "*" {
		t.Fatalf("presigner If-None-Match = %#v, want *", presigner.requests)
	}
}

func TestCapsuleAccessAllowsOmittedDigestList(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	presigner := &capsulePresignerFake{}
	handler := capsuleAccessHandler(presigner, capsuleOwnershipFake{owned: true}, now)

	response := serveCapsuleAccessRequest(handler, true, `{"intent":"pull"}`)
	if response.Code < http.StatusBadRequest || response.Code >= http.StatusInternalServerError {
		t.Fatalf("omitted object list status = %d, want 4xx; body: %s", response.Code, response.Body.String())
	}
	if len(presigner.requests) != 0 {
		t.Fatalf("omitted object list presigns = %#v; want none", presigner.requests)
	}
}

type capsulePresignerFake struct {
	requests []capsulePresignRequest
}

type capsulePresignRequest struct {
	key         string
	method      string
	ifNoneMatch string
}

func (presigner *capsulePresignerFake) PresignGetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	presigner.requests = append(presigner.requests, capsulePresignRequest{key: *input.Key, method: http.MethodGet})
	return &v4.PresignedHTTPRequest{URL: "https://capsules.example.test/get", Method: http.MethodGet, SignedHeader: http.Header{}}, nil
}

func (presigner *capsulePresignerFake) PresignPutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	ifNoneMatch := ""
	if input.IfNoneMatch != nil {
		ifNoneMatch = *input.IfNoneMatch
	}
	presigner.requests = append(presigner.requests, capsulePresignRequest{key: *input.Key, method: http.MethodPut, ifNoneMatch: ifNoneMatch})
	return &v4.PresignedHTTPRequest{URL: "https://capsules.example.test/put", Method: http.MethodPut, SignedHeader: http.Header{"If-None-Match": []string{"*"}}}, nil
}

type capsuleOwnershipFake struct {
	owned bool
}

func (ownership capsuleOwnershipFake) OwnsCapsule(context.Context, string, string) (bool, error) {
	return ownership.owned, nil
}

func capsuleAccessHandler(presigner *capsulePresignerFake, ownership capsuleOwnershipFake, now time.Time) http.Handler {
	return controlplane.NewHandler(controlplane.Config{
		CapsulePresigner: presigner, CapsuleBucket: "capsules", CapsuleOwnership: ownership,
		CapsuleAccessTTL: 15 * time.Minute, Verifier: verifierFake{}, Users: &usersFake{},
		UserIDs: &idsFake{values: []string{"user-1"}}, RequestIDs: &idsFake{values: []string{"request-capsule-access"}},
		DefaultRegion: "us-east-1", Now: func() time.Time { return now },
	})
}

func serveCapsuleAccessRequest(handler http.Handler, withAuth bool, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/capsule-access", bytes.NewBufferString(body))
	if withAuth {
		request.Header.Set("Authorization", "Bearer valid-token")
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
