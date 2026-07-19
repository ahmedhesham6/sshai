package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/db"
)

func TestCapsuleTagHTTPRetagsAndReplaysSameDigestIdempotently(t *testing.T) {
	firstTime := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	repository := &capsuleTagRepositoryFake{}
	now := firstTime
	handler := capsuleTagHandler(repository, capsuleOwnershipFake{owned: true}, func() time.Time { return now }, 4)
	firstDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	first := serveCapsuleTagRequest(handler, http.MethodPut, "/v1/capsules/agents/tags/stable", `{"digest":"`+firstDigest+`"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first publish = status:%d body:%s", first.Code, first.Body.String())
	}
	now = now.Add(time.Minute)
	replay := serveCapsuleTagRequest(handler, http.MethodPut, "/v1/capsules/agents/tags/stable", `{"digest":"`+firstDigest+`"}`)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = status:%d body:%s", replay.Code, replay.Body.String())
	}
	var replayed contracts.CapsuleTag
	if err := json.NewDecoder(replay.Body).Decode(&replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.UpdatedAt.Equal(firstTime) {
		t.Fatalf("idempotent update time = %s, want %s", replayed.UpdatedAt, firstTime)
	}
	now = now.Add(time.Minute)
	retag := serveCapsuleTagRequest(handler, http.MethodPut, "/v1/capsules/agents/tags/stable", `{"digest":"`+secondDigest+`"}`)
	if retag.Code != http.StatusOK {
		t.Fatalf("retag = status:%d body:%s", retag.Code, retag.Body.String())
	}
	get := serveCapsuleTagRequest(handler, http.MethodGet, "/v1/capsules/agents/tags/stable", "")
	var resolved contracts.CapsuleTag
	if get.Code != http.StatusOK || json.NewDecoder(get.Body).Decode(&resolved) != nil || resolved.Digest != secondDigest || !resolved.UpdatedAt.Equal(now) {
		t.Fatalf("resolved tag = status:%d record:%#v body:%s", get.Code, resolved, get.Body.String())
	}
}

func TestCapsuleTagHTTPUsesNotFoundConventionForForeignTagAndDigest(t *testing.T) {
	repository := &capsuleTagRepositoryFake{getErr: db.ErrReferenceNotOwned}
	handler := capsuleTagHandler(repository, capsuleOwnershipFake{owned: false}, time.Now, 2)
	get := serveCapsuleTagRequest(handler, http.MethodGet, "/v1/capsules/agents/tags/stable", "")
	if get.Code != http.StatusNotFound {
		t.Fatalf("foreign tag status = %d, want 404; body:%s", get.Code, get.Body.String())
	}
	put := serveCapsuleTagRequest(handler, http.MethodPut, "/v1/capsules/agents/tags/stable", `{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if put.Code != http.StatusNotFound || repository.putCalls != 0 {
		t.Fatalf("foreign digest = status:%d put-calls:%d body:%s", put.Code, repository.putCalls, put.Body.String())
	}
}

type capsuleTagRepositoryFake struct {
	record   db.CapsuleTag
	getErr   error
	putCalls int
}

func (repository *capsuleTagRepositoryFake) PutCapsuleTag(_ context.Context, ownerID, name, tag, digest string, updatedAt time.Time) (db.CapsuleTag, error) {
	repository.putCalls++
	if repository.record.Digest == digest {
		return repository.record, nil
	}
	repository.record = db.CapsuleTag{OwnerUserID: ownerID, Name: name, Tag: tag, Digest: digest, UpdatedAt: updatedAt}
	return repository.record, nil
}

func (repository *capsuleTagRepositoryFake) GetCapsuleTag(context.Context, string, string, string) (db.CapsuleTag, error) {
	if repository.getErr != nil {
		return db.CapsuleTag{}, repository.getErr
	}
	if repository.record.Digest == "" {
		return db.CapsuleTag{}, errors.New("missing fake record")
	}
	return repository.record, nil
}

func capsuleTagHandler(repository *capsuleTagRepositoryFake, ownership capsuleOwnershipFake, now func() time.Time, requests int) http.Handler {
	userIDs, requestIDs := make([]string, requests), make([]string, requests)
	for index := range requests {
		userIDs[index], requestIDs[index] = "user-1", "request-capsule-tag"
	}
	return controlplane.NewHandler(controlplane.Config{
		CapsuleTags: repository, CapsuleOwnership: ownership, Verifier: verifierFake{}, Users: &usersFake{},
		UserIDs: &idsFake{values: userIDs}, RequestIDs: &idsFake{values: requestIDs}, DefaultRegion: "eu-central-1", Now: now,
	})
}

func serveCapsuleTagRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
