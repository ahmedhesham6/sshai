package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"golang.org/x/crypto/ssh"
)

func TestGenerateDedicatedSSHKeyCreatesParseablePairWithoutClobbering(t *testing.T) {
	sshDirectory := filepath.Join(t.TempDir(), ".ssh")
	key, err := generateDedicatedSSHKey(sshDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if key.Label != dedicatedSSHKeyName || key.PrivateKeyPath != filepath.Join(sshDirectory, dedicatedSSHKeyName) {
		t.Fatalf("generated key = %#v", key)
	}
	privateContent, err := os.ReadFile(key.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParsePrivateKey(privateContent); err != nil {
		t.Fatalf("parse generated private key: %v", err)
	}
	publicContent, err := os.ReadFile(key.PrivateKeyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, fingerprint, ok := parseEd25519PublicKey(publicContent)
	if !ok || publicKey != key.PublicKey || fingerprint != key.Fingerprint {
		t.Fatalf("generated public metadata = key:%q fingerprint:%q ok:%t", publicKey, fingerprint, ok)
	}
	assertMode(t, key.PrivateKeyPath, 0o600)
	before := append([]byte(nil), privateContent...)
	if _, err := generateDedicatedSSHKey(sshDirectory); err == nil {
		t.Fatal("dedicated key generation replaced an existing key")
	}
	after, err := os.ReadFile(key.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("existing dedicated private key was changed")
	}
}

func TestCLISSHSetupIsIdempotentUploadsPublicKeyAndRendersAllEnvironments(t *testing.T) {
	root := t.TempDir()
	configDirectory := filepath.Join(root, ".config", "devm")
	sshDirectory := filepath.Join(root, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEd25519KeyPair(t, sshDirectory, "id_older", "older key")
	publicKey, fingerprint := writeEd25519KeyPair(t, sshDirectory, "id_work", "work laptop")
	now := time.Now().Truncate(time.Second)
	older := now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(sshDirectory, "id_older"), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(sshDirectory, "id_work"), now, now); err != nil {
		t.Fatal(err)
	}
	primaryConfig := filepath.Join(sshDirectory, "config")
	userConfig := "# user-owned\nHost github.com\n    User git\n"
	if err := os.WriteFile(primaryConfig, []byte(userConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	accessToken := testAccessToken(t, time.Now().Add(time.Hour))
	if err := persistCredentials(configDirectory, loginCredentials{accessToken: accessToken, refreshToken: "REFRESH_SECRET"}); err != nil {
		t.Fatal(err)
	}

	var uploaded atomic.Int32
	var stateMu sync.Mutex
	registered := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+accessToken {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/environments":
			if request.Method != http.MethodGet || request.URL.Query().Get("pageSize") != "50" {
				t.Errorf("Environment request = method:%s query:%s", request.Method, request.URL.RawQuery)
			}
			cursor := request.URL.Query().Get("cursor")
			if cursor == "" {
				next := "page-02"
				_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{
					Items:      []contracts.Environment{{Id: "env_02", Slug: "zeta", Lifecycle: contracts.Active}},
					NextCursor: &next,
				})
				return
			}
			if cursor != "page-02" {
				t.Errorf("Environment cursor = %q", cursor)
			}
			_ = json.NewEncoder(response).Encode(contracts.EnvironmentPage{Items: []contracts.Environment{
				{Id: "env_01", Slug: "api-dev", Lifecycle: contracts.Active},
				{Id: "env_deleted", Slug: "old", Lifecycle: contracts.Deleted},
			}})
		case "/v1/ssh-keys":
			switch request.Method {
			case http.MethodGet:
				stateMu.Lock()
				isRegistered := registered
				stateMu.Unlock()
				page := contracts.SSHKeyPage{}
				if isRegistered {
					page.Items = []contracts.SSHKey{{
						Id: "key_01", Label: "id_work", Algorithm: contracts.SshEd25519,
						Fingerprint: fingerprint, PublicKey: publicKey, CreatedAt: time.Now(),
					}}
				}
				_ = json.NewEncoder(response).Encode(page)
			case http.MethodPost:
				if key := request.Header.Get("Idempotency-Key"); len(key) < 16 || len(key) > 128 {
					t.Errorf("idempotency key = %q", key)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if bytes.Contains(body, []byte("private key contents")) || bytes.Contains(body, []byte("REFRESH_SECRET")) {
					t.Fatalf("upload leaked private material: %s", body)
				}
				var input contracts.CreateSSHKeyJSONRequestBody
				if err := json.Unmarshal(body, &input); err != nil {
					t.Error(err)
					return
				}
				if input.Label != "id_work" || input.PublicKey != publicKey {
					t.Errorf("SSH key input = %#v", input)
				}
				stateMu.Lock()
				registered = true
				stateMu.Unlock()
				uploaded.Add(1)
				response.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(response).Encode(contracts.SSHKey{
					Id: "key_01", Label: input.Label, Algorithm: contracts.SshEd25519,
					Fingerprint: fingerprint, PublicKey: publicKey, CreatedAt: time.Now(),
				})
			default:
				http.Error(response, "method", http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	application := cli{
		output: &output, clientID: "client_public_01", controlPlaneURL: server.URL + "/v1",
		httpClient: server.Client(), now: time.Now,
		configDirectory:  func() (string, error) { return configDirectory, nil },
		sshDirectory:     func() (string, error) { return sshDirectory, nil },
		newRefreshClient: func(string) (tokenRefresher, error) { return &singleUseRefreshFake{}, nil },
	}
	for range 2 {
		if err := application.run(context.Background(), []string{"ssh", "setup"}); err != nil {
			t.Fatalf("devm ssh setup: %v", err)
		}
	}
	if uploaded.Load() != 1 {
		t.Fatalf("SSH key uploads = %d, want 1", uploaded.Load())
	}
	if !strings.Contains(output.String(), "Selected most-recently-used SSH key "+filepath.Join(sshDirectory, "id_work")) ||
		!strings.Contains(output.String(), "--identity-file PATH") {
		t.Fatalf("multiple-key selection note = %q", output.String())
	}

	primary, err := os.ReadFile(primaryConfig)
	if err != nil {
		t.Fatal(err)
	}
	include := "Include " + filepath.Join(configDirectory, "ssh", "config")
	if strings.Count(string(primary), include) != 1 || !strings.Contains(string(primary), userConfig) {
		t.Fatalf("primary SSH config was not preserved: %q", primary)
	}
	info, err := os.Stat(primaryConfig)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("primary config mode = %o", info.Mode().Perm())
	}
	owned, err := os.ReadFile(filepath.Join(configDirectory, "ssh", "config"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(owned)
	if strings.Count(config, "Host ") != 2 || !strings.Contains(config, "Host api-dev\n") ||
		!strings.Contains(config, "Host zeta\n") || strings.Contains(config, "Host old\n") ||
		strings.Count(config, "IdentityFile "+filepath.Join(sshDirectory, "id_work")) != 2 {
		t.Fatalf("managed SSH config:\n%s", config)
	}
}
