package application_test

import (
	"context"
	"time"

	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

type recordingUploadVerifier struct {
	calls []application.VerifyUploadInput
	sizes map[string]int64
	err   error
}

func (verifier *recordingUploadVerifier) Verify(_ context.Context, input application.VerifyUploadInput) (application.VerifiedUpload, error) {
	verifier.calls = append(verifier.calls, input)
	if verifier.err != nil {
		return application.VerifiedUpload{}, verifier.err
	}
	size := verifier.sizes[input.Digest]
	intent, err := domain.ReserveUploadIntent(domain.UploadIntentSnapshot{
		ID: "upload-verified", OwnerUserID: input.OwnerUserID, Kind: input.Kind, Digest: input.Digest, SizeBytes: size,
		ObjectKey: "uploads/" + string(input.Kind) + "/verified", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		return application.VerifiedUpload{}, err
	}
	return application.VerifiedUpload{Intent: intent, ObjectKey: "objects/final"}, nil
}
