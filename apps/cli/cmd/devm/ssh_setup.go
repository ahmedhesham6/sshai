package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"golang.org/x/crypto/ssh"
)

const (
	dedicatedSSHKeyName = "id_devm"
	setupPageSize       = 10
	maximumSetupPages   = 1000
)

type sshSetupCommand struct {
	controlPlaneURL string
	httpClient      *http.Client
	tokens          accessTokenSource
	configDirectory string
	sshDirectory    string
	output          io.Writer
}

func (command sshSetupCommand) run(ctx context.Context, identityOverride string) error {
	if err := command.validate(); err != nil {
		return err
	}
	accessToken, err := command.tokens.FreshAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate SSH setup: %w", err)
	}
	if accessToken == "" {
		return errors.New("authenticate SSH setup: access token is unavailable")
	}
	client := cloneProxyHTTPClient(command.httpClient)
	api, err := contracts.NewClientWithResponses(command.controlPlaneURL, contracts.WithHTTPClient(client))
	if err != nil {
		return errors.New("configure SSH setup: control plane URL is invalid")
	}
	environments, err := listSetupEnvironments(ctx, api, accessToken)
	if err != nil {
		return err
	}
	registeredKeys, err := listSetupSSHKeys(ctx, api, accessToken)
	if err != nil {
		return err
	}
	localKeys, err := discoverEd25519Keys(command.sshDirectory)
	if err != nil {
		return fmt.Errorf("discover SSH keys: %w", err)
	}
	selected, multiple, err := chooseLocalSSHKey(localKeys, identityOverride)
	if err != nil {
		return err
	}
	generated := false
	if len(localKeys) == 0 {
		selected, err = generateDedicatedSSHKey(command.sshDirectory)
		if err != nil {
			return fmt.Errorf("generate dedicated SSH key: %w", err)
		}
		generated = true
	}

	entries := make([]sshEnvironmentConfig, 0, len(environments))
	for _, environment := range environments {
		if environment.Lifecycle == contracts.Deleted {
			continue
		}
		entries = append(entries, sshEnvironmentConfig{
			Alias: environment.Slug, EnvironmentID: environment.Id, IdentityFile: selected.PrivateKeyPath,
		})
	}
	knownHostsPath := filepath.Join(command.configDirectory, "known_hosts")
	config, err := renderSSHConfig(entries, knownHostsPath)
	if err != nil {
		return err
	}
	registered, err := ensureSSHKeyRegistered(ctx, api, accessToken, selected, registeredKeys)
	if err != nil {
		return err
	}
	ownedConfigPath := filepath.Join(command.configDirectory, "ssh", "config")
	if err := writeOwnedSSHConfig(command.configDirectory, config); err != nil {
		return fmt.Errorf("write managed SSH config: %w", err)
	}
	primaryConfigPath := filepath.Join(command.sshDirectory, "config")
	if err := ensureSSHInclude(primaryConfigPath, ownedConfigPath); err != nil {
		return err
	}

	output := command.output
	if output == nil {
		output = io.Discard
	}
	if generated {
		fmt.Fprintf(output, "Generated dedicated devm SSH key at %s.\n", selected.PrivateKeyPath)
	}
	if multiple {
		fmt.Fprintf(output, "Selected most-recently-used SSH key %s; override with --identity-file PATH.\n", selected.PrivateKeyPath)
	}
	if registered {
		fmt.Fprintf(output, "Registered SSH public key %s.\n", selected.Fingerprint)
	}
	_, err = fmt.Fprintf(output, "Configured SSH access for %d Environments.\n", len(entries))
	if err != nil {
		return errors.New("write SSH setup result")
	}
	return nil
}

func (command sshSetupCommand) validate() error {
	if command.tokens == nil || command.configDirectory == "" || command.sshDirectory == "" {
		return errors.New("configure SSH setup: command is incomplete")
	}
	if !filepath.IsAbs(command.configDirectory) || !filepath.IsAbs(command.sshDirectory) {
		return errors.New("configure SSH setup: local directories must be absolute")
	}
	_, err := secureControlPlaneURL(command.controlPlaneURL)
	return err
}

func chooseLocalSSHKey(keys []localSSHKey, identityOverride string) (localSSHKey, bool, error) {
	if identityOverride != "" {
		cleaned := filepath.Clean(identityOverride)
		for _, key := range keys {
			if identityOverride == key.Label || cleaned == filepath.Clean(key.PrivateKeyPath) {
				return key, false, nil
			}
		}
		return localSSHKey{}, false, fmt.Errorf("select SSH key: --identity-file %q is not a discovered Ed25519 key", identityOverride)
	}
	if len(keys) == 0 {
		return localSSHKey{}, false, nil
	}
	selected := keys[0]
	for _, key := range keys[1:] {
		if key.LastUsed.After(selected.LastUsed) ||
			(key.LastUsed.Equal(selected.LastUsed) && key.PrivateKeyPath < selected.PrivateKeyPath) {
			selected = key
		}
	}
	return selected, len(keys) > 1, nil
}

func generateDedicatedSSHKey(sshDirectory string) (localSSHKey, error) {
	return generateDedicatedSSHKeyWithCreator(sshDirectory, createExclusiveSSHFile)
}

type exclusiveSSHFileCreator func(*anchoredDirectory, string, []byte, os.FileMode) error

func generateDedicatedSSHKeyWithCreator(sshDirectory string, createFile exclusiveSSHFileCreator) (localSSHKey, error) {
	directory, err := openOwnedDirectory(sshDirectory)
	if err != nil {
		return localSSHKey{}, err
	}
	defer directory.Close()
	for _, name := range []string{dedicatedSSHKeyName, dedicatedSSHKeyName + ".pub"} {
		if _, err := directory.root.Lstat(name); err == nil {
			return localSSHKey{}, fmt.Errorf("refusing to replace existing %s", filepath.Join(sshDirectory, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return localSSHKey{}, err
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return localSSHKey{}, err
	}
	privateBlock, err := ssh.MarshalPrivateKey(private, "devm")
	if err != nil {
		return localSSHKey{}, err
	}
	privateContent := pem.EncodeToMemory(privateBlock)
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return localSSHKey{}, err
	}
	publicKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic)))
	if err := createFile(directory, dedicatedSSHKeyName, privateContent, 0o600); err != nil {
		return localSSHKey{}, err
	}
	privateInfo, err := directory.root.Lstat(dedicatedSSHKeyName)
	if err != nil || !privateInfo.Mode().IsRegular() {
		return localSSHKey{}, errors.New("generated private SSH key changed before its public half was written")
	}
	if err := createFile(directory, dedicatedSSHKeyName+".pub", []byte(publicKey+" devm\n"), 0o644); err != nil {
		return localSSHKey{}, errors.Join(err, removeSSHFileIfSame(directory, dedicatedSSHKeyName, privateInfo))
	}
	return localSSHKey{
		Label: dedicatedSSHKeyName, PrivateKeyPath: filepath.Join(sshDirectory, dedicatedSSHKeyName),
		PublicKey: publicKey, Fingerprint: ssh.FingerprintSHA256(sshPublic), LastUsed: time.Now(),
	}, nil
}

func removeSSHFileIfSame(directory *anchoredDirectory, name string, expected os.FileInfo) error {
	current, err := directory.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return errors.New("generated private SSH key changed before rollback")
	}
	if err := directory.root.Remove(name); err != nil {
		return err
	}
	return syncAnchoredDirectory(directory)
}

func createExclusiveSSHFile(directory *anchoredDirectory, name string, content []byte, mode os.FileMode) error {
	file, err := directory.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	cleanup := func() {
		current, currentErr := directory.root.Lstat(name)
		if statErr == nil && currentErr == nil && os.SameFile(info, current) {
			_ = directory.root.Remove(name)
		}
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		cleanup()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return err
	}
	handle, err := directory.root.Open(".")
	if err != nil {
		cleanup()
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil || closeErr != nil {
		cleanup()
		return errors.Join(syncErr, closeErr)
	}
	return nil
}

func listSetupEnvironments(ctx context.Context, api *contracts.ClientWithResponses, token string) ([]contracts.Environment, error) {
	pageSize := contracts.PageSize(setupPageSize)
	return paginateSetup(ctx, "list Environments for SSH setup", func(requestContext context.Context, cursor *contracts.Cursor) (setupPage[contracts.Environment], error) {
		response, err := api.ListEnvironmentsWithResponse(requestContext, &contracts.ListEnvironmentsParams{
			Cursor: cursor, PageSize: &pageSize,
		}, bearerRequestEditor(token))
		if err != nil {
			return setupPage[contracts.Environment]{}, err
		}
		page := setupPage[contracts.Environment]{status: response.StatusCode()}
		if response.JSON200 != nil {
			page.items, page.next, page.valid = response.JSON200.Items, response.JSON200.NextCursor, true
		}
		return page, nil
	})
}

func listSetupSSHKeys(ctx context.Context, api *contracts.ClientWithResponses, token string) ([]contracts.SSHKey, error) {
	pageSize := contracts.PageSize(setupPageSize)
	return paginateSetup(ctx, "list SSH keys", func(requestContext context.Context, cursor *contracts.Cursor) (setupPage[contracts.SSHKey], error) {
		response, err := api.ListSSHKeysWithResponse(requestContext, &contracts.ListSSHKeysParams{
			Cursor: cursor, PageSize: &pageSize,
		}, bearerRequestEditor(token))
		if err != nil {
			return setupPage[contracts.SSHKey]{}, err
		}
		page := setupPage[contracts.SSHKey]{status: response.StatusCode()}
		if response.JSON200 != nil {
			page.items, page.next, page.valid = response.JSON200.Items, response.JSON200.NextCursor, true
		}
		return page, nil
	})
}

type setupPage[T any] struct {
	items  []T
	next   *string
	status int
	valid  bool
}

func paginateSetup[T any](ctx context.Context, action string, fetch func(context.Context, *contracts.Cursor) (setupPage[T], error)) ([]T, error) {
	var items []T
	var cursor *contracts.Cursor
	seen := make(map[string]struct{})
	for range maximumSetupPages {
		requestContext, cancel := context.WithTimeout(ctx, proxyRequestTimeout)
		page, err := fetch(requestContext, cursor)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			return nil, errors.New(action + ": control plane is unavailable")
		}
		if page.status != http.StatusOK || !page.valid {
			return nil, fmt.Errorf("%s: control plane returned HTTP %d", action, page.status)
		}
		items = append(items, page.items...)
		if page.next == nil {
			return items, nil
		}
		next := *page.next
		if next == "" {
			return nil, errors.New(action + ": control plane returned an invalid cursor")
		}
		if _, duplicate := seen[next]; duplicate {
			return nil, errors.New(action + ": control plane repeated a cursor")
		}
		seen[next] = struct{}{}
		cursor = (*contracts.Cursor)(&next)
	}
	return nil, errors.New(action + ": pagination limit exceeded")
}

func ensureSSHKeyRegistered(ctx context.Context, api *contracts.ClientWithResponses, token string, selected localSSHKey, registered []contracts.SSHKey) (bool, error) {
	for _, key := range registered {
		if key.Fingerprint == selected.Fingerprint && key.PublicKey == selected.PublicKey {
			return false, nil
		}
	}
	digest := sha256.Sum256([]byte(selected.Fingerprint + "\x00" + selected.PublicKey))
	idempotencyKey := "ssh-setup-" + hex.EncodeToString(digest[:])[:32]
	requestContext, cancel := context.WithTimeout(ctx, proxyRequestTimeout)
	response, err := api.CreateSSHKeyWithResponse(
		requestContext,
		&contracts.CreateSSHKeyParams{IdempotencyKey: contracts.IdempotencyKey(idempotencyKey)},
		contracts.CreateSSHKeyJSONRequestBody{Label: setupSSHKeyLabel(selected.Label), PublicKey: selected.PublicKey},
		bearerRequestEditor(token),
	)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, context.Cause(ctx)
		}
		return false, errors.New("register SSH public key: control plane is unavailable")
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil {
		return false, fmt.Errorf("register SSH public key: control plane returned HTTP %d", response.StatusCode())
	}
	created := response.JSON201
	if created.Algorithm != contracts.SshEd25519 || created.Fingerprint != selected.Fingerprint || created.PublicKey != selected.PublicKey {
		return false, errors.New("register SSH public key: control plane returned invalid key metadata")
	}
	return true, nil
}

func setupSSHKeyLabel(value string) string {
	if value == "" {
		return "devm"
	}
	for utf8.RuneCountInString(value) > 80 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func bearerRequestEditor(token string) contracts.RequestEditorFn {
	return func(_ context.Context, request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

func writeOwnedSSHConfig(configDirectory, config string) error {
	directory, err := openOwnedDirectory(configDirectory)
	if err != nil {
		return err
	}
	defer directory.Close()
	sshDirectory, err := directory.ownedChild("ssh")
	if err != nil {
		return err
	}
	defer sshDirectory.Close()
	return sshDirectory.writePrivate("config", []byte(config))
}
