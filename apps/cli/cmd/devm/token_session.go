package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
)

const maxTokenFileSize = 64 << 10
const accessTokenRefreshSkew = 2 * time.Minute

type tokenRefresher interface {
	Refresh(context.Context, auth.RefreshCredential) (auth.TokenPair, error)
}

type tokenSession struct {
	configDirectory string
	refresher       tokenRefresher
	now             func() time.Time
}

func newTokenSession(configDirectory string, refresher tokenRefresher, now func() time.Time) *tokenSession {
	return &tokenSession{configDirectory: configDirectory, refresher: refresher, now: now}
}

func (session *tokenSession) FreshAccessToken(ctx context.Context) (string, error) {
	token, _, _, err := session.freshAccessToken(ctx)
	return token, err
}

func (session *tokenSession) freshAccessToken(ctx context.Context) (string, time.Time, bool, error) {
	if session == nil || session.refresher == nil || session.now == nil {
		return "", time.Time{}, false, errors.New("load token session: session is not initialized")
	}
	authDirectory, err := openPrivateAuthDirectory(session.configDirectory)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("load token session: %w", err)
	}
	defer authDirectory.Close()
	lock, err := acquireTokenFileLock(ctx, authDirectory)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("lock token session: %w", err)
	}
	defer lock.Close()
	credentials, expiresAt, err := loadTokenCredentials(authDirectory)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("load token session: %w", err)
	}
	if expiresAt.After(session.now().Add(accessTokenRefreshSkew)) {
		return credentials.accessToken, expiresAt, false, nil
	}
	refreshCredential, err := auth.NewRefreshCredential(credentials.refreshToken)
	if err != nil {
		return "", time.Time{}, false, errors.New("refresh token session: stored refresh credential is invalid")
	}
	rotatedPair, err := session.refresher.Refresh(ctx, refreshCredential)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("refresh token session: %w", err)
	}
	rotated := loginCredentials{accessToken: rotatedPair.AccessToken(), refreshToken: rotatedPair.RefreshToken()}
	rotatedExpiry, err := accessTokenExpiry(rotated.accessToken)
	if err != nil || rotated.refreshToken == "" || !rotatedExpiry.After(session.now().Add(accessTokenRefreshSkew)) {
		return "", time.Time{}, false, errors.New("refresh token session: rotated credentials are invalid")
	}
	if !lock.StillCurrent() {
		return "", time.Time{}, false, errors.New("refresh token session: lock file changed before mutation")
	}
	if err := writeTokenCredentials(authDirectory, rotated); err != nil {
		return "", time.Time{}, false, fmt.Errorf("persist rotated token session: %w", err)
	}
	return rotated.accessToken, rotatedExpiry, true, nil
}

func (session *tokenSession) Delete(ctx context.Context) error {
	if session == nil {
		return errors.New("delete token session: session is not initialized")
	}
	authDirectory, err := openPrivateAuthDirectory(session.configDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token session: %w", err)
	}
	defer authDirectory.Close()
	lock, err := acquireTokenFileLock(ctx, authDirectory)
	if err != nil {
		return fmt.Errorf("delete token session: %w", err)
	}
	defer lock.Close()
	info, err := authDirectory.root.Lstat("tokens.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("delete token session: credentials are not a private regular file")
	}
	if !lock.StillCurrent() {
		return errors.New("delete token session: lock file changed before mutation")
	}
	if err := authDirectory.root.Remove("tokens.json"); err != nil {
		return fmt.Errorf("delete token session: remove credentials: %w", err)
	}
	return syncAnchoredDirectory(authDirectory)
}

func (session *tokenSession) Stored() (bool, error) {
	if session == nil {
		return false, errors.New("inspect token session: session is not initialized")
	}
	authDirectory, err := openPrivateAuthDirectory(session.configDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect token session: %w", err)
	}
	defer authDirectory.Close()
	info, err := authDirectory.root.Lstat("tokens.json")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return false, errors.New("inspect token session: credentials are not a private regular file")
	}
	return true, nil
}

func persistCredentials(configDirectory string, credentials loginCredentials) error {
	return persistCredentialsContext(context.Background(), configDirectory, credentials)
}

func persistCredentialsContext(ctx context.Context, configDirectory string, credentials loginCredentials) error {
	state, err := openOwnedDirectory(configDirectory)
	if err != nil {
		return fmt.Errorf("open private local state: %w", err)
	}
	defer state.Close()
	authDirectory, err := state.ownedChild("auth")
	if err != nil {
		return fmt.Errorf("open private auth state: %w", err)
	}
	defer authDirectory.Close()
	lock, err := acquireTokenFileLock(ctx, authDirectory)
	if err != nil {
		return fmt.Errorf("lock token session: %w", err)
	}
	defer lock.Close()
	if !lock.StillCurrent() {
		return errors.New("persist token session: lock file changed before mutation")
	}
	return writeTokenCredentials(authDirectory, credentials)
}

func openPrivateAuthDirectory(configDirectory string) (*anchoredDirectory, error) {
	directory, err := openAnchoredDirectory(filepath.Join(configDirectory, "auth"), false, 0)
	if err != nil {
		return nil, err
	}
	info, err := directory.root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		directory.Close()
		return nil, errors.New("auth directory is not private")
	}
	return directory, nil
}

func loadTokenCredentials(directory *anchoredDirectory) (loginCredentials, time.Time, error) {
	content, info, err := directory.readRegular("tokens.json", maxTokenFileSize)
	if err != nil {
		return loginCredentials{}, time.Time{}, err
	}
	if info.Mode().Perm() != 0o600 {
		return loginCredentials{}, time.Time{}, errors.New("credentials permissions must be 0600")
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return loginCredentials{}, time.Time{}, errors.New("credentials JSON is malformed")
	}
	if !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return loginCredentials{}, time.Time{}, errors.New("credentials JSON has trailing content")
	}
	if payload.AccessToken == "" || payload.RefreshToken == "" {
		return loginCredentials{}, time.Time{}, errors.New("credentials are incomplete")
	}
	expiresAt, err := accessTokenExpiry(payload.AccessToken)
	if err != nil {
		return loginCredentials{}, time.Time{}, err
	}
	return loginCredentials{accessToken: payload.AccessToken, refreshToken: payload.RefreshToken}, expiresAt, nil
}

func writeTokenCredentials(directory *anchoredDirectory, credentials loginCredentials) error {
	if credentials.accessToken == "" || credentials.refreshToken == "" {
		return errors.New("credentials are incomplete")
	}
	payload := struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}{AccessToken: credentials.accessToken, RefreshToken: credentials.refreshToken}
	content, err := json.Marshal(payload)
	if err != nil {
		return errors.New("encode credentials")
	}
	return directory.writePrivate("tokens.json", append(content, '\n'))
}

func accessTokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return time.Time{}, errors.New("access token is not a compact JWT")
	}
	if _, err := decodeJWTObject(parts[0]); err != nil {
		return time.Time{}, errors.New("access token header is malformed")
	}
	claims, err := decodeJWTObject(parts[1])
	if err != nil {
		return time.Time{}, errors.New("access token claims are malformed")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) == 0 || len(signature) > maxTokenFileSize {
		return time.Time{}, errors.New("access token signature is malformed")
	}
	expiration, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, errors.New("access token expiration is missing")
	}
	seconds, err := expiration.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("access token expiration is malformed")
	}
	return time.Unix(seconds, 0), nil
}

func decodeJWTObject(segment string) (map[string]any, error) {
	content, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil || len(content) == 0 || len(content) > maxTokenFileSize {
		return nil, errors.New("JWT segment is malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("JWT segment is malformed")
	}
	if !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return nil, errors.New("JWT segment has trailing content")
	}
	return object, nil
}

func syncAnchoredDirectory(directory *anchoredDirectory) error {
	handle, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
