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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

	readiness, err := client.WaitForReadiness(context.Background(), target, guest.ReadinessDataMounted)
	if err != nil {
		t.Fatalf("wait for readiness: %v", err)
	}
	if readiness.Snapshot.Level != guest.ReadinessDataMounted || readiness.Snapshot.BootID != "boot-1" || readiness.PrivateIPv4 != target.PrivateIPv4 {
		t.Fatalf("readiness = %+v", readiness)
	}
	if operations.readinessCalls != 2 {
		t.Fatalf("readiness calls = %d, want 2", operations.readinessCalls)
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
		{name: "shutdown preparation", run: func() error { return client.PrepareShutdown(context.Background(), target) }},
		{name: "Capsule Lock Materialization", run: func() error {
			results, err := client.ApplyMaterialization(context.Background(), materialization)
			if err == nil && (len(results) != 1 || results[0].ComponentID != "config:editor") {
				return errors.New("Materialization results were not preserved")
			}
			return err
		}},
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

	_, err := client.WaitForReadiness(context.Background(), control.Target{EnvironmentID: "environment-1"}, guest.ReadinessAllocated)
	if err == nil {
		t.Fatal("wrong-CA client unexpectedly authenticated")
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
	handler, err := control.NewServer(control.ServerConfig{EnvironmentID: "environment-1", ClientIdentity: "spiffe://devm/workflows"}, operations)
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
		Endpoint: server.URL, CertificateFile: selected.certificateFile, PrivateKeyFile: selected.keyFile,
		CAFile: serverPKI.caFile, PollInterval: time.Millisecond,
	})
	if err != nil {
		server.Close()
		t.Fatalf("construct client: %v", err)
	}
	return client, server.Close
}

func testPKI(t *testing.T, name string) pkiFiles {
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
	identity, _ := url.Parse("spiffe://devm/workflows")
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
