package control_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/apps/guest/control"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestClientAndServerRoundTrip(t *testing.T) {
	target := control.Target{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
		ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
	}
	operations := &fakeOperations{
		readiness: []control.ReadinessStatus{
			{Snapshot: guest.ReadinessSnapshot{RuntimeID: target.RuntimeID, BootID: "boot-1", RuntimeSequence: 1, Level: guest.ReadinessAllocated, ObservedAt: time.Unix(1, 0).UTC()}, PrivateIPv4: target.PrivateIPv4},
			{Snapshot: guest.ReadinessSnapshot{RuntimeID: target.RuntimeID, BootID: "boot-1", RuntimeSequence: 1, Level: guest.ReadinessDataMounted, ObservedAt: time.Unix(2, 0).UTC()}, PrivateIPv4: target.PrivateIPv4},
		},
		sshIdentity:      guest.SSHHostIdentityStatus{Fingerprint: "SHA256:host", Materialized: true},
		materializations: []profile.ProfileMaterializationResult{{ComponentID: "config:editor", LockID: "lock-1"}},
		activity:         control.ActivitySnapshot{RuntimeID: target.RuntimeID, ObservedAt: time.Unix(3, 0).UTC(), GuestSequence: 7, SSHConnections: 1},
	}
	client, closeServer := newMTLSTestPair(t, operations, testPKI(t, "trusted"), testPKI(t, "unused"), false)
	defer closeServer()

	readiness, err := client.ReadReadiness(context.Background(), target)
	if err != nil {
		t.Fatalf("wait for readiness: %v", err)
	}
	if readiness.Snapshot.Level != guest.ReadinessAllocated || readiness.Snapshot.BootID != "boot-1" || readiness.PrivateIPv4 != target.PrivateIPv4 {
		t.Fatalf("readiness = %+v", readiness)
	}
	if operations.readinessCalls != 1 {
		t.Fatalf("readiness calls = %d, want one current observation", operations.readinessCalls)
	}

	seed := guest.ProjectSeedApplicationInput{RepositoryURL: "https://example.invalid/repository.git", BaseRevision: "0123456789012345678901234567890123456789"}
	materialization := control.MaterializationRequest{
		Target: target,
		Lock:   domain.CapsuleLockSnapshot{ID: "lock-1", EnvironmentID: target.EnvironmentID},
		Intent: profile.IntentReconcile,
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "Project Seed apply", run: func() error {
			return client.ApplyProjectSeed(context.Background(), control.ProjectSeedRequest{Target: target, Seed: seed})
		}},
		{name: "SSH host identity restore", run: func() error {
			status, err := client.RestoreSSHHostIdentity(context.Background(), target)
			if err == nil && status.Fingerprint != "SHA256:host" {
				return errors.New("SSH host identity response was not preserved")
			}
			return err
		}},
		{name: "SSH public key reconcile", run: func() error { return client.ReconcileSSHKeys(context.Background(), target) }},
		{name: "managed configuration reconcile", run: func() error { return client.ReconcileManagedConfiguration(context.Background(), target) }},
		{name: "Capsule Lock Materialization", run: func() error {
			results, err := client.ApplyMaterialization(context.Background(), materialization)
			if err == nil && (len(results) != 1 || results[0].ComponentID != "config:editor") {
				return errors.New("Materialization results were not preserved")
			}
			return err
		}},
		{name: "shutdown preparation", run: func() error { return client.PrepareShutdown(context.Background(), target) }},
		{name: "Activity Snapshot", run: func() error {
			snapshot, err := client.ReadActivitySnapshot(context.Background(), target)
			if err == nil && (snapshot.GuestSequence != 7 || snapshot.SSHConnections != 1) {
				return errors.New("Activity Snapshot was not preserved")
			}
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if operations.seed.Seed.RepositoryURL != seed.RepositoryURL || operations.materialization.Lock.ID != "lock-1" {
		t.Fatalf("requests were not preserved: seed=%+v materialization=%+v", operations.seed, operations.materialization)
	}
}

func TestServerRejectsClientSignedByWrongCA(t *testing.T) {
	trusted := testPKI(t, "trusted")
	wrong := testPKI(t, "wrong")
	client, closeServer := newMTLSTestPair(t, &fakeOperations{}, trusted, wrong, true)
	defer closeServer()

	_, err := client.ReadReadiness(context.Background(), control.Target{EnvironmentID: "environment-1", PrivateIPv4: "10.0.0.8"})
	if err == nil {
		t.Fatal("wrong-CA client unexpectedly authenticated")
	}
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("wrong-CA error = %T %v, want permanent classified error", err, err)
	}
}

func TestServerRejectsCertificateClaimForAnotherEnvironment(t *testing.T) {
	_, err := control.NewServer(control.ServerConfig{
		Target:         control.Target{OwnerUserID: "user-1", EnvironmentID: "environment-2", RuntimeID: "runtime-2", ProviderID: "instance-2", PrivateIPv4: "10.0.0.9"},
		ClientIdentity: "spiffe://devm/workflows/environment/environment-1",
	}, &fakeOperations{})
	if err == nil {
		t.Fatal("guest accepted a client certificate identity scoped to another Environment")
	}
}

func TestEnvironmentScopedCertificateCannotOperateAnotherGuest(t *testing.T) {
	certificate := testPKIWithIdentity(t, "trusted", "spiffe://devm/workflows/environment/environment-1")
	handler, err := control.NewServer(control.ServerConfig{
		Target:         control.Target{OwnerUserID: "user-2", EnvironmentID: "environment-2", RuntimeID: "runtime-2", ProviderID: "instance-2", PrivateIPv4: "10.9.8.7"},
		ClientIdentity: "spiffe://devm/workflows/environment/environment-2",
	}, &fakeOperations{})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := control.LoadServerTLSConfig(certificate.certificateFile, certificate.keyFile, certificate.caFile)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	dialed := ""
	client, err := control.NewClient(control.ClientConfig{
		Port: 9443, ServerName: "example.com", CertificateFile: certificate.certificateFile,
		PrivateKeyFile: certificate.keyFile, CAFile: certificate.caFile,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = address
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	target := control.Target{EnvironmentID: "environment-2", PrivateIPv4: "10.9.8.7"}
	_, err = client.ReadReadiness(context.Background(), target)
	if err == nil {
		t.Fatal("Environment 1 certificate operated Environment 2 guest")
	}
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("cross-Environment rejection = %T %v, want permanent", err, err)
	}
	if dialed != "10.9.8.7:9443" {
		t.Fatalf("dial address = %q, want request target address", dialed)
	}
}

func TestOperationErrorClassificationSurvivesHTTP(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "transient", err: classifiedTestError{message: "retry", transient: true}, transient: true},
		{name: "permanent", err: classifiedTestError{message: "reject"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := &classifiedOperations{err: test.err}
			client, closeServer := newMTLSTestPair(t, operations, testPKI(t, "trusted"), testPKI(t, "unused"), false)
			defer closeServer()
			err := client.ReconcileSSHKeys(context.Background(), control.Target{
				OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
				ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
			})
			var classified interface{ Transient() bool }
			if !errors.As(err, &classified) || classified.Transient() != test.transient {
				t.Fatalf("error = %T %v, transient=%v", err, err, classified)
			}
		})
	}
}

func TestMaterializationPreservesBlockedResultsWithPermanentError(t *testing.T) {
	operations := &classifiedOperations{
		fakeOperations: fakeOperations{materializations: []profile.ProfileMaterializationResult{{ComponentID: "config:editor", Operation: profile.OperationRequiresInput}}},
		err:            classifiedTestError{message: "approval required"},
	}
	client, closeServer := newMTLSTestPair(t, operations, testPKI(t, "trusted"), testPKI(t, "unused"), false)
	defer closeServer()
	target := control.Target{OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1", ProviderID: "instance-1", PrivateIPv4: "10.0.0.8"}
	results, err := client.ApplyMaterialization(context.Background(), control.MaterializationRequest{Target: target})
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("materialization error = %T %v, want permanent", err, err)
	}
	if len(results) != 1 || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("materialization results = %#v, want requires-input result", results)
	}
}

func TestClientClassifiesRetryableHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, closeServer := newStatusTestClient(t, status)
			defer closeServer()
			_, err := client.ReadReadiness(context.Background(), control.Target{EnvironmentID: "environment-1", PrivateIPv4: "10.0.0.8"})
			var classified interface{ Transient() bool }
			if !errors.As(err, &classified) || !classified.Transient() {
				t.Fatalf("HTTP %d error = %T %v, want transient", status, err, err)
			}
		})
	}
}

func TestTLSPrivateKeyMustBeTrusted0600File(t *testing.T) {
	pki := testPKI(t, "permissions")
	if err := os.Chmod(pki.keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := control.LoadServerTLSConfig(pki.certificateFile, pki.keyFile, pki.caFile); err == nil {
		t.Fatal("server accepted a non-private TLS key")
	}
	if _, err := control.NewClient(control.ClientConfig{
		CertificateFile: pki.certificateFile, PrivateKeyFile: pki.keyFile, CAFile: pki.caFile,
	}); err == nil {
		t.Fatal("client accepted a non-private TLS key")
	}
	if err := os.Chmod(pki.keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	keyLink := filepath.Join(t.TempDir(), "key.pem")
	if err := os.Symlink(pki.keyFile, keyLink); err != nil {
		t.Fatal(err)
	}
	if _, err := control.LoadServerTLSConfig(pki.certificateFile, keyLink, pki.caFile); err == nil {
		t.Fatal("server followed a TLS private-key symlink")
	}
}

func TestDirectoryCertificateSourceSelectsByEnvironment(t *testing.T) {
	pki := testPKI(t, "directory")
	certificateDirectory, keyDirectory := t.TempDir(), t.TempDir()
	copyTestFile(t, pki.certificateFile, filepath.Join(certificateDirectory, "environment-1.pem"), 0o600)
	copyTestFile(t, pki.keyFile, filepath.Join(keyDirectory, "environment-1.pem"), 0o600)
	source, err := control.NewDirectoryClientCertificateSource(certificateDirectory, keyDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ClientCertificate(context.Background(), control.Target{EnvironmentID: "environment-1"}); err != nil {
		t.Fatalf("load scoped certificate: %v", err)
	}
	_, err = source.ClientCertificate(context.Background(), control.Target{EnvironmentID: "environment-2"})
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("missing Environment certificate = %T %v, want permanent", err, err)
	}
}

func TestShutdownQuiescesMutationsBeforePreparation(t *testing.T) {
	operations := &quiesceOperations{started: make(chan struct{}), release: make(chan struct{})}
	client, closeServer := newMTLSTestPair(t, operations, testPKI(t, "trusted"), testPKI(t, "unused"), false)
	defer closeServer()
	target := control.Target{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
		ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
	}
	seedDone := make(chan error, 1)
	go func() {
		seedDone <- client.ApplyProjectSeed(context.Background(), control.ProjectSeedRequest{Target: target})
	}()
	<-operations.started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- client.PrepareShutdown(context.Background(), target) }()

	deadline := time.Now().Add(time.Second)
	for {
		err := client.ReconcileSSHKeys(context.Background(), target)
		if err != nil {
			var classified interface{ Transient() bool }
			if !errors.As(err, &classified) || classified.Transient() {
				t.Fatalf("quiescing rejection = %T %v, want permanent", err, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not reject new mutation while quiescing")
		}
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown preparation completed before in-flight seed: %v", err)
	default:
	}
	close(operations.release)
	if err := <-seedDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if strings.Join(operations.order, ",") != "seed,shutdown" {
		t.Fatalf("operation order = %v, want seed then shutdown", operations.order)
	}
}

func TestMisdirectedShutdownDoesNotQuiesceCurrentRuntime(t *testing.T) {
	operations := &fakeOperations{}
	client, closeServer := newMTLSTestPair(t, operations, testPKI(t, "trusted"), testPKI(t, "unused"), false)
	defer closeServer()
	target := control.Target{OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1", ProviderID: "instance-1", PrivateIPv4: "10.0.0.8"}
	wrong := target
	wrong.RuntimeID = "runtime-stale"
	err := client.PrepareShutdown(context.Background(), wrong)
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("misdirected shutdown error = %T %v, want permanent", err, err)
	}
	if err := client.ReconcileSSHKeys(context.Background(), target); err != nil {
		t.Fatalf("valid mutation after rejected shutdown: %v", err)
	}
}

type fakeOperations struct {
	mu               sync.Mutex
	readiness        []control.ReadinessStatus
	readinessCalls   int
	seed             control.ProjectSeedRequest
	sshIdentity      guest.SSHHostIdentityStatus
	materialization  control.MaterializationRequest
	materializations []profile.ProfileMaterializationResult
	activity         control.ActivitySnapshot
}

type classifiedTestError struct {
	message   string
	transient bool
}

func (err classifiedTestError) Error() string   { return err.message }
func (err classifiedTestError) Transient() bool { return err.transient }

type classifiedOperations struct {
	fakeOperations
	err error
}

func (operations *classifiedOperations) ReconcileSSHKeys(context.Context, control.Target) error {
	return operations.err
}

func (operations *classifiedOperations) ApplyMaterialization(_ context.Context, request control.MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	operations.materialization = request
	return operations.materializations, operations.err
}

type quiesceOperations struct {
	fakeOperations
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	order   []string
}

func (operations *quiesceOperations) ApplyProjectSeed(context.Context, control.ProjectSeedRequest) error {
	close(operations.started)
	<-operations.release
	operations.mu.Lock()
	operations.order = append(operations.order, "seed")
	operations.mu.Unlock()
	return nil
}

func (operations *quiesceOperations) PrepareShutdown(context.Context, control.Target) error {
	operations.mu.Lock()
	operations.order = append(operations.order, "shutdown")
	operations.mu.Unlock()
	return nil
}

func (fake *fakeOperations) Readiness(context.Context, control.Target) (control.ReadinessStatus, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	index := fake.readinessCalls
	if index >= len(fake.readiness) {
		index = len(fake.readiness) - 1
	}
	fake.readinessCalls++
	if index < 0 {
		return control.ReadinessStatus{}, errors.New("readiness unavailable")
	}
	return fake.readiness[index], nil
}

func (fake *fakeOperations) ApplyProjectSeed(_ context.Context, request control.ProjectSeedRequest) error {
	fake.seed = request
	return nil
}

func (fake *fakeOperations) RestoreSSHHostIdentity(context.Context, control.Target) (guest.SSHHostIdentityStatus, error) {
	return fake.sshIdentity, nil
}

func (*fakeOperations) ReconcileSSHKeys(context.Context, control.Target) error { return nil }

func (*fakeOperations) ReconcileManagedConfiguration(context.Context, control.Target) error {
	return nil
}

func (*fakeOperations) PrepareShutdown(context.Context, control.Target) error { return nil }

func (fake *fakeOperations) ApplyMaterialization(_ context.Context, request control.MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	fake.materialization = request
	return fake.materializations, nil
}

func (fake *fakeOperations) ReadActivitySnapshot(context.Context, control.Target) (control.ActivitySnapshot, error) {
	return fake.activity, nil
}

type pkiFiles struct {
	caFile, certificateFile, keyFile string
}

func newMTLSTestPair(t *testing.T, operations control.Operations, serverPKI, clientPKI pkiFiles, useWrongClient bool) (*control.Client, func()) {
	t.Helper()
	serverTLS, err := control.LoadServerTLSConfig(serverPKI.certificateFile, serverPKI.keyFile, serverPKI.caFile)
	if err != nil {
		t.Fatalf("load server TLS: %v", err)
	}
	handler, err := control.NewServer(control.ServerConfig{
		Target:         control.Target{OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1", ProviderID: "instance-1", PrivateIPv4: "10.0.0.8"},
		ClientIdentity: "spiffe://devm/workflows/environment/environment-1",
	}, operations)
	if err != nil {
		t.Fatalf("construct server: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = serverTLS
	server.StartTLS()

	selected := serverPKI
	if useWrongClient {
		selected = clientPKI
	}
	client, err := control.NewClient(control.ClientConfig{
		Port: 9443, ServerName: "example.com", CertificateFile: selected.certificateFile, PrivateKeyFile: selected.keyFile,
		CAFile: serverPKI.caFile,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	})
	if err != nil {
		server.Close()
		t.Fatalf("construct client: %v", err)
	}
	return client, server.Close
}

func newStatusTestClient(t *testing.T, status int) (*control.Client, func()) {
	t.Helper()
	pki := testPKI(t, "status")
	serverTLS, err := control.LoadServerTLSConfig(pki.certificateFile, pki.keyFile, pki.caFile)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(status)
	}))
	server.TLS = serverTLS
	server.StartTLS()
	client, err := control.NewClient(control.ClientConfig{
		Port: 9443, ServerName: "example.com", CertificateFile: pki.certificateFile,
		PrivateKeyFile: pki.keyFile, CAFile: pki.caFile,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server.Close
}

func testPKI(t *testing.T, name string) pkiFiles {
	return testPKIWithIdentity(t, name, "spiffe://devm/workflows/environment/environment-1")
}

func testPKIWithIdentity(t *testing.T, name, identityURI string) pkiFiles {
	t.Helper()
	directory := t.TempDir()
	now := time.Now()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name + " CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leafPublic, leafPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := url.Parse(identityURI)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name + " peer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames: []string{"example.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{identity},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(directory, "ca.pem")
	certificateFile := filepath.Join(directory, "certificate.pem")
	keyFile := filepath.Join(directory, "private-key.pem")
	writePEM(t, caFile, "CERTIFICATE", caDER)
	writePEM(t, certificateFile, "CERTIFICATE", leafDER)
	privateDER, err := x509.MarshalPKCS8PrivateKey(leafPrivate)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyFile, "PRIVATE KEY", privateDER)
	return pkiFiles{caFile: caFile, certificateFile: certificateFile, keyFile: keyFile}
}

func writePEM(t *testing.T, name, kind string, content []byte) {
	t.Helper()
	if err := os.WriteFile(name, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: content}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyTestFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, mode); err != nil {
		t.Fatal(err)
	}
}
