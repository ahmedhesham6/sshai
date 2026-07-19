package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLoginPrintsOnlyPromptAndAtomicallyPersistsPrivateTokens(t *testing.T) {
	configDirectory := t.TempDir()
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access-secret", refreshToken: "refresh-secret"},
	}
	authDirectory := filepath.Join(configDirectory, "auth")
	if err := os.Mkdir(authDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDirectory, "tokens.json"), []byte("old credentials"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer

	if err := runLogin(context.Background(), flow, configDirectory, &output); err != nil {
		t.Fatalf("run login: %v", err)
	}
	if got, want := output.String(), "Verification URI: https://auth.example/device\nUser code: ABCD-EFGH\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "device-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("output exposed %q", secret)
		}
	}

	assertMode(t, authDirectory, 0o700)
	tokenPath := filepath.Join(authDirectory, "tokens.json")
	assertMode(t, tokenPath, 0o600)
	assertMode(t, filepath.Join(authDirectory, "tokens.lock"), 0o600)
	content, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "access-secret") || !strings.Contains(string(content), "refresh-secret") {
		t.Fatalf("token file is incomplete: %s", content)
	}
	entries, err := os.ReadDir(authDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "tokens.json" || entries[1].Name() != "tokens.lock" {
		t.Fatalf("atomic write left temporary files: %v", entries)
	}
}

func TestRunLoginDoesNotPersistFailedOrCanceledAuthorization(t *testing.T) {
	for _, failure := range []error{context.Canceled, errors.New("authorization denied")} {
		t.Run(failure.Error(), func(t *testing.T) {
			configDirectory := t.TempDir()
			flow := stubLoginFlow{prompt: loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"}, err: failure}
			if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); !errors.Is(err, failure) {
				t.Fatalf("RunLogin() error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(configDirectory, "auth", "tokens.json")); !os.IsNotExist(err) {
				t.Fatal("failed login persisted tokens")
			}
		})
	}
}

func TestRunLoginRejectsSymlinkedAuthDirectory(t *testing.T) {
	configDirectory := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(configDirectory, "auth")); err != nil {
		t.Fatal(err)
	}
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"},
	}
	if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); err == nil {
		t.Fatal("login accepted a symlinked auth directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "tokens.json")); !os.IsNotExist(err) {
		t.Fatal("tokens escaped through symlinked auth directory")
	}
}

func TestRunLoginRejectsSymlinkedConfigDirectoryBeforeWritingOutside(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	configDirectory := filepath.Join(parent, "devm")
	if err := os.Symlink(outside, configDirectory); err != nil {
		t.Fatal(err)
	}
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"},
	}
	if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); err == nil {
		t.Fatal("login accepted a symlinked config directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "auth")); !os.IsNotExist(err) {
		t.Fatal("login wrote outside through symlinked config directory")
	}
}

func TestRunLoginRejectsSymlinkedConfigAncestorBeforeWritingOutside(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	linkedParent := filepath.Join(parent, "config-parent")
	if err := os.Symlink(outside, linkedParent); err != nil {
		t.Fatal(err)
	}
	configDirectory := filepath.Join(linkedParent, "devm")
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"},
	}
	if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); err == nil {
		t.Fatal("login accepted a symlinked config ancestor")
	}
	if _, err := os.Stat(filepath.Join(outside, "devm")); !os.IsNotExist(err) {
		t.Fatal("login wrote outside through a symlinked config ancestor")
	}
}

func TestRunLoginCreatesMissingPrivateConfigAncestors(t *testing.T) {
	configDirectory := filepath.Join(t.TempDir(), ".config", "devm")
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"},
	}
	if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); err != nil {
		t.Fatalf("run login: %v", err)
	}
	assertMode(t, filepath.Join(configDirectory, "auth"), 0o700)
	assertMode(t, filepath.Join(configDirectory, "auth", "tokens.json"), 0o600)
}

func TestRunLoginAtomicallyReplacesTokenSymlinkWithoutFollowingIt(t *testing.T) {
	configDirectory := t.TempDir()
	authDirectory := filepath.Join(configDirectory, "auth")
	if err := os.Mkdir(authDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideToken := filepath.Join(t.TempDir(), "outside-token")
	if err := os.WriteFile(outsideToken, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(authDirectory, "tokens.json")
	if err := os.Symlink(outsideToken, tokenPath); err != nil {
		t.Fatal(err)
	}
	flow := stubLoginFlow{
		prompt:      loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"},
		credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"},
	}
	if err := runLogin(context.Background(), flow, configDirectory, &bytes.Buffer{}); err != nil {
		t.Fatalf("run login: %v", err)
	}
	outsideContent, err := os.ReadFile(outsideToken)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideContent) != "sentinel" {
		t.Fatal("token replacement followed the existing symlink")
	}
	info, err := os.Lstat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("token path was not replaced with a regular file")
	}
}

func TestCLIRoutesLoginAndRequiresPublicClientID(t *testing.T) {
	configDirectory := t.TempDir()
	createdWith := ""
	application := cli{
		output:          &bytes.Buffer{},
		clientID:        "client_123",
		configDirectory: func() (string, error) { return configDirectory, nil },
		newLoginFlow: func(clientID string) (loginFlow, error) {
			createdWith = clientID
			return stubLoginFlow{prompt: loginPrompt{userCode: "ABCD-EFGH", verificationURI: "https://auth.example/device"}, credentials: loginCredentials{accessToken: "access", refreshToken: "refresh"}}, nil
		},
	}
	if err := application.run(context.Background(), []string{"login"}); err != nil {
		t.Fatalf("route login: %v", err)
	}
	if createdWith != "client_123" {
		t.Fatalf("flow client ID = %q", createdWith)
	}
	application.clientID = ""
	if err := application.run(context.Background(), []string{"login"}); err == nil {
		t.Fatal("login accepted a missing DEVM_WORKOS_CLIENT_ID")
	}
}

type stubLoginFlow struct {
	prompt      loginPrompt
	credentials loginCredentials
	err         error
}

func (flow stubLoginFlow) Login(_ context.Context, display func(loginPrompt) error) (loginCredentials, error) {
	if err := display(flow.prompt); err != nil {
		return loginCredentials{}, err
	}
	return flow.credentials, flow.err
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
