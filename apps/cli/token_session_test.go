package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
)

func TestTokenSessionSerializesSingleUseRefreshAndPersistsRotatedPair(t *testing.T) {
	configDirectory := t.TempDir()
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	expired := testAccessToken(t, now.Add(-time.Minute))
	rotated := testAccessToken(t, now.Add(time.Hour))
	if err := persistCredentials(configDirectory, loginCredentials{accessToken: expired, refreshToken: "refresh_old"}); err != nil {
		t.Fatalf("persist initial credentials: %v", err)
	}
	rotatedPair, err := auth.NewTokenPair(rotated, "refresh_new")
	if err != nil {
		t.Fatal(err)
	}
	refresher := &singleUseRefreshFake{rotated: rotatedPair}
	start := make(chan struct{})
	results := make(chan struct {
		token string
		err   error
	}, 3)
	for range 3 {
		session := newTokenSession(configDirectory, refresher, func() time.Time { return now })
		go func() {
			<-start
			token, err := session.FreshAccessToken(context.Background())
			results <- struct {
				token string
				err   error
			}{token, err}
		}()
	}
	close(start)
	for range 3 {
		result := <-results
		if result.err != nil || result.token != rotated {
			t.Fatalf("fresh access result = token-match:%t error:%v", result.token == rotated, result.err)
		}
	}
	if calls := refresher.callCount(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
	content, err := os.ReadFile(filepath.Join(configDirectory, "auth", "tokens.json"))
	if err != nil {
		t.Fatalf("read rotated tokens: %v", err)
	}
	if strings.Contains(string(content), "refresh_old") || !strings.Contains(string(content), "refresh_new") || !strings.Contains(string(content), rotated) {
		t.Fatal("rotated token file did not atomically replace both credentials")
	}
	info, err := os.Stat(filepath.Join(configDirectory, "auth", "tokens.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token permissions = %v, %v", info.Mode().Perm(), err)
	}
}

func TestTokenSessionSerializesRefreshAcrossProcesses(t *testing.T) {
	configDirectory := t.TempDir()
	if err := persistCredentials(configDirectory, loginCredentials{
		accessToken: testAccessToken(t, time.Now().Add(-time.Minute)), refreshToken: "refresh_old",
	}); err != nil {
		t.Fatal(err)
	}
	rotatedAccess := testAccessToken(t, time.Now().Add(time.Hour))
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if refreshCalls.Add(1) != 1 {
			http.Error(response, "refresh token already used", http.StatusBadRequest)
			return
		}
		time.Sleep(50 * time.Millisecond)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]string{
			"access_token": rotatedAccess, "refresh_token": "refresh_new",
		})
	}))
	defer server.Close()
	commands := make([]*exec.Cmd, 3)
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestTokenSessionCrossProcessHelper$", "-test.count=1")
		command.Env = append(os.Environ(),
			"DEVM_TEST_TOKEN_HELPER=1",
			"DEVM_TEST_CONFIG_DIRECTORY="+configDirectory,
			"DEVM_TEST_REFRESH_ENDPOINT="+server.URL,
		)
		commands[index] = command
		if err := command.Start(); err != nil {
			t.Fatalf("start helper %d: %v", index, err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("helper %d: %v", index, err)
		}
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("cross-process refresh calls = %d, want 1", calls)
	}
}

func TestTokenSessionCrossProcessHelper(t *testing.T) {
	if os.Getenv("DEVM_TEST_TOKEN_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	session := newTokenSession(
		os.Getenv("DEVM_TEST_CONFIG_DIRECTORY"),
		helperHTTPRefresher{endpoint: os.Getenv("DEVM_TEST_REFRESH_ENDPOINT")},
		time.Now,
	)
	if _, err := session.FreshAccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type helperHTTPRefresher struct{ endpoint string }

func (refresher helperHTTPRefresher) Refresh(ctx context.Context, _ auth.RefreshCredential) (auth.TokenPair, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, refresher.endpoint, nil)
	if err != nil {
		return auth.TokenPair{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return auth.TokenPair{}, err
	}
	defer response.Body.Close()
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&payload) != nil {
		return auth.TokenPair{}, errors.New("test refresh failed")
	}
	return auth.NewTokenPair(payload.AccessToken, payload.RefreshToken)
}

func TestTokenSessionFailsClosedOnMalformedOrUnsafeState(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "malformed JSON", setup: func(t *testing.T, authDirectory string) {
			writeTokenFixture(t, authDirectory, []byte(`{"access_token":`), 0o600)
		}},
		{name: "malformed JWT", setup: func(t *testing.T, authDirectory string) {
			writeTokenFixture(t, authDirectory, tokenJSON(t, "not-a-jwt", "refresh"), 0o600)
		}},
		{name: "malformed JWT header", setup: func(t *testing.T, authDirectory string) {
			payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
			writeTokenFixture(t, authDirectory, tokenJSON(t, "%%%."+payload+".signature", "refresh"), 0o600)
		}},
		{name: "JWT payload trailing JSON", setup: func(t *testing.T, authDirectory string) {
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
			payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d} trailing`, now.Add(time.Hour).Unix())))
			signature := base64.RawURLEncoding.EncodeToString([]byte("signature"))
			writeTokenFixture(t, authDirectory, tokenJSON(t, header+"."+payload+"."+signature, "refresh"), 0o600)
		}},
		{name: "malformed JWT signature", setup: func(t *testing.T, authDirectory string) {
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
			payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
			writeTokenFixture(t, authDirectory, tokenJSON(t, header+"."+payload+".%%%", "refresh"), 0o600)
		}},
		{name: "open permissions", setup: func(t *testing.T, authDirectory string) {
			writeTokenFixture(t, authDirectory, tokenJSON(t, testAccessToken(t, now.Add(time.Hour)), "refresh"), 0o644)
		}},
		{name: "symlink token file", setup: func(t *testing.T, authDirectory string) {
			target := filepath.Join(t.TempDir(), "target")
			writeTokenFixture(t, filepath.Dir(target), tokenJSON(t, testAccessToken(t, now.Add(time.Hour)), "refresh"), 0o600)
			if err := os.Rename(filepath.Join(filepath.Dir(target), "tokens.json"), target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(authDirectory, "tokens.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configDirectory := t.TempDir()
			authDirectory := filepath.Join(configDirectory, "auth")
			if err := os.Mkdir(authDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, authDirectory)
			session := newTokenSession(configDirectory, &singleUseRefreshFake{}, func() time.Time { return now })
			if token, err := session.FreshAccessToken(context.Background()); err == nil || token != "" {
				t.Fatalf("unsafe session result = token:%q error:%v", token, err)
			}
		})
	}
}

func TestTokenSessionDeleteRemovesOnlyAnchoredCredentialFile(t *testing.T) {
	configDirectory := t.TempDir()
	credentials := loginCredentials{accessToken: testAccessToken(t, time.Now().Add(time.Hour)), refreshToken: "refresh"}
	if err := persistCredentials(configDirectory, credentials); err != nil {
		t.Fatal(err)
	}
	session := newTokenSession(configDirectory, &singleUseRefreshFake{}, time.Now)
	if err := session.Delete(context.Background()); err != nil {
		t.Fatalf("delete token session: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(configDirectory, "auth", "tokens.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tokens still exist after delete: %v", err)
	}
}

type singleUseRefreshFake struct {
	mu      sync.Mutex
	rotated auth.TokenPair
	calls   int
}

func (fake *singleUseRefreshFake) Refresh(_ context.Context, credential auth.RefreshCredential) (auth.TokenPair, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if credential == (auth.RefreshCredential{}) || fake.calls > 1 {
		return auth.TokenPair{}, errors.New("single-use refresh token rejected")
	}
	return fake.rotated, nil
}

func (fake *singleUseRefreshFake) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func testAccessToken(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	encode := func(value any) string {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(content)
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(map[string]int64{"exp": expiresAt.Unix()}) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature"))
}

func tokenJSON(t *testing.T, accessToken, refreshToken string) []byte {
	t.Helper()
	content, err := json.Marshal(map[string]string{"access_token": accessToken, "refresh_token": refreshToken})
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func writeTokenFixture(t *testing.T, directory string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(directory, "tokens.json")
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoginCredentialsNeverRenderTokens(t *testing.T) {
	credentials := loginCredentials{accessToken: "ACCESS_SECRET", refreshToken: "REFRESH_SECRET"}
	for _, rendered := range []string{fmt.Sprint(credentials), fmt.Sprintf("%#v", credentials)} {
		if strings.Contains(rendered, "ACCESS_SECRET") || strings.Contains(rendered, "REFRESH_SECRET") {
			t.Fatalf("credentials rendered a token: %q", rendered)
		}
	}
}
