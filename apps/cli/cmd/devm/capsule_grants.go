package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const maxCapsuleGrantErrorBytes = 4 << 10
const maxCapsulePublicationObjectBytes = 256 << 20

var capsuleGrantDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var errCapsuleCapabilityAlreadyExists = errors.New("Capsule object already exists")

// capsuleGrantProvider adapts the generated control-plane client to OCI's
// per-object capability seam. Presigned URLs stay inside the returned
// closures and are never included in errors.
type capsuleGrantProvider struct {
	api        *contracts.ClientWithResponses
	httpClient *http.Client
	token      string
}

func (provider capsuleGrantProvider) Grant(ctx context.Context, request oci.GrantRequest) (oci.Grant, error) {
	if provider.api == nil || provider.httpClient == nil || provider.token == "" {
		return oci.Grant{}, errors.New("mint Capsule object grant: provider is not configured")
	}
	kind, digest, err := capsuleAccessObject(request.OwnerID, request.Key)
	if err != nil {
		return oci.Grant{}, err
	}
	intent := contracts.Pull
	expectedMethod := contracts.GET
	if request.Operation == oci.GrantWrite {
		intent, expectedMethod = contracts.Push, contracts.PUT
	} else if request.Operation != oci.GrantRead {
		return oci.Grant{}, fmt.Errorf("mint Capsule object grant: unsupported operation %q", request.Operation)
	}
	response, err := provider.api.CreateCapsuleAccessWithResponse(ctx, contracts.CapsuleAccessRequest{
		Intent: intent, Objects: []contracts.CapsuleAccessObject{{Kind: kind, Digest: digest}},
	}, bearerRequestEditor(provider.token))
	if err != nil {
		return oci.Grant{}, fmt.Errorf("mint Capsule object grant: %w", err)
	}
	if response == nil || response.StatusCode() != http.StatusOK || response.JSON200 == nil || len(response.JSON200.Grants) != 1 {
		return oci.Grant{}, errors.New("mint Capsule object grant: control plane returned an invalid response")
	}
	issued := response.JSON200.Grants[0]
	if issued.Method != expectedMethod {
		return oci.Grant{}, errors.New("mint Capsule object grant: control plane returned the wrong method")
	}
	parsedURL, err := url.Parse(issued.Url)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" {
		return oci.Grant{}, errors.New("mint Capsule object grant: control plane returned an invalid capability")
	}
	headers := make(http.Header, len(issued.Headers))
	for name, value := range issued.Headers {
		if strings.ContainsAny(name+value, "\r\n") {
			return oci.Grant{}, errors.New("mint Capsule object grant: control plane returned invalid headers")
		}
		headers.Set(name, value)
	}
	grant := oci.Grant{ExpiresAt: issued.ExpiresAt, URL: issued.Url}
	if request.Operation == oci.GrantRead {
		grant.Read = func(ctx context.Context) (io.ReadCloser, error) {
			return readCapsuleCapability(ctx, provider.httpClient, issued.Url, headers)
		}
	} else {
		grant.Write = func(ctx context.Context, reader io.Reader, size int64) error {
			return provider.writeGrantedObject(ctx, request, issued.Url, headers, reader, size)
		}
	}
	return grant, nil
}

func (provider capsuleGrantProvider) writeGrantedObject(ctx context.Context, request oci.GrantRequest, capability string, headers http.Header, reader io.Reader, size int64) error {
	if size < 0 || size > maxCapsulePublicationObjectBytes {
		return errors.New("write Capsule object: size is outside the supported range")
	}
	content, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil || int64(len(content)) != size {
		return errors.New("write Capsule object: content size does not match the declaration")
	}
	err = writeCapsuleCapability(ctx, provider.httpClient, capability, headers, bytes.NewReader(content), size)
	if !errors.Is(err, errCapsuleCapabilityAlreadyExists) {
		return err
	}
	existingGrant, err := provider.Grant(ctx, oci.GrantRequest{OwnerID: request.OwnerID, Key: request.Key, Operation: oci.GrantRead})
	if err != nil {
		return errors.New("write Capsule object: verify existing immutable object")
	}
	existing, err := existingGrant.Read(ctx)
	if err != nil {
		return errors.New("write Capsule object: verify existing immutable object")
	}
	existingContent, readErr := io.ReadAll(io.LimitReader(existing, size+1))
	closeErr := existing.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(existingContent, content) {
		return errors.New("write Capsule object: existing immutable object does not match publication")
	}
	return nil
}

func capsuleAccessObject(ownerID, key string) (contracts.CapsuleAccessObjectKind, string, error) {
	prefix := "owner/" + ownerID + "/"
	if ownerID == "" || !strings.HasPrefix(key, prefix) || strings.ContainsAny(key, "\r\n") {
		return "", "", errors.New("mint Capsule object grant: object is outside the owner prefix")
	}
	remainder := strings.TrimPrefix(key, prefix)
	parts := strings.Split(remainder, "/")
	kind := contracts.Blob
	var algorithm, encoded string
	switch {
	case len(parts) == 4 && parts[0] == "index" && parts[1] == "manifest":
		kind, algorithm, encoded = contracts.Index, parts[2], parts[3]
	case len(parts) == 3 && parts[0] == "blobs":
		algorithm, encoded = parts[1], parts[2]
	default:
		return "", "", errors.New("mint Capsule object grant: object key is invalid")
	}
	digest := algorithm + ":" + encoded
	if !capsuleGrantDigestPattern.MatchString(digest) {
		return "", "", errors.New("mint Capsule object grant: object digest is invalid")
	}
	return kind, digest, nil
}

func readCapsuleCapability(ctx context.Context, client *http.Client, capability string, headers http.Header) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, capability, nil)
	if err != nil {
		return nil, errors.New("read Capsule object: build capability request")
	}
	request.Header = headers.Clone()
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read Capsule object: %w", ctx.Err())
		}
		return nil, errors.New("read Capsule object: capability request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardCapsuleErrorBody(response.Body)
		return nil, fmt.Errorf("read Capsule object: unexpected status %d", response.StatusCode)
	}
	return response.Body, nil
}

func writeCapsuleCapability(ctx context.Context, client *http.Client, capability string, headers http.Header, reader io.Reader, size int64) error {
	if size < 0 {
		return errors.New("write Capsule object: size must not be negative")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, capability, io.NopCloser(io.LimitReader(reader, size)))
	if err != nil {
		return errors.New("write Capsule object: build capability request")
	}
	request.Header = headers.Clone()
	request.ContentLength = size
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("write Capsule object: %w", ctx.Err())
		}
		return errors.New("write Capsule object: capability request failed")
	}
	if response.StatusCode == http.StatusPreconditionFailed {
		discardCapsuleErrorBody(response.Body)
		return errCapsuleCapabilityAlreadyExists
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardCapsuleErrorBody(response.Body)
		return fmt.Errorf("write Capsule object: unexpected status %d", response.StatusCode)
	}
	return nil
}

func discardCapsuleErrorBody(body io.ReadCloser) {
	defer body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxCapsuleGrantErrorBytes))
}

var _ oci.GrantProvider = capsuleGrantProvider{}
