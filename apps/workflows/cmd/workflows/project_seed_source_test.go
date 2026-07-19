package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type failingProjectSeedObjectClient struct {
	output *s3.GetObjectOutput
	err    error
}

func (client failingProjectSeedObjectClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return client.output, client.err
}

func TestProjectSeedObjectSourceClassifiesMissingObjectPermanentAndEnvironmentalFailureTransient(t *testing.T) {
	tests := []struct {
		name      string
		client    failingProjectSeedObjectClient
		transient bool
	}{
		{name: "missing immutable object", client: failingProjectSeedObjectClient{err: &s3types.NoSuchKey{}}, transient: false},
		{name: "malformed object response", client: failingProjectSeedObjectClient{output: &s3.GetObjectOutput{}}, transient: false},
		{name: "environmental S3 failure", client: failingProjectSeedObjectClient{err: &smithy.GenericAPIError{Code: "InternalError", Message: "retry"}}, transient: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := s3ProjectSeedObjectSource{client: test.client, bucket: "seed-bucket"}
			_, err := source.ReadProjectSeedObject(t.Context(), "user-1", domain.UploadSeedManifest, "sha256:"+string(make([]byte, 64)))
			if err == nil {
				t.Fatal("ReadProjectSeedObject() error = nil")
			}
			var classified interface{ Transient() bool }
			if !errors.As(err, &classified) || classified.Transient() != test.transient {
				t.Fatalf("classification = %T %v, want transient=%t", err, err, test.transient)
			}
		})
	}
}
