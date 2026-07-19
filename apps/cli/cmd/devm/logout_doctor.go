package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/pelletier/go-toml/v2"
)

func (application cli) runLogout(ctx context.Context, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: devm logout")
	}
	if application.configDirectory == nil {
		return errors.New("logout: local state directory is unavailable")
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return errors.New("logout: resolve local state directory")
	}
	session := newTokenSession(configDirectory, nil, application.now)
	existed, err := session.Stored()
	if err != nil {
		return err
	}
	if err := session.Delete(ctx); err != nil {
		return err
	}
	message := "No local login session existed."
	if existed {
		message = "Logged out; local login session removed."
	}
	if _, err := fmt.Fprintln(writerOrDiscard(application.output), message); err != nil {
		return errors.New("write logout result")
	}
	return nil
}

type doctorLevel string

const (
	doctorPass doctorLevel = "pass"
	doctorWarn doctorLevel = "warn"
	doctorFail doctorLevel = "fail"
)

type doctorResult struct {
	name   string
	level  doctorLevel
	detail string
}

func (application cli) runDoctor(ctx context.Context, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: devm doctor")
	}
	var configDirectory, sshDirectory string
	var configErr, sshErr error
	if application.configDirectory == nil {
		configErr = errors.New("local state directory resolver is unavailable")
	} else {
		configDirectory, configErr = application.configDirectory()
	}
	if application.sshDirectory == nil {
		sshErr = errors.New("SSH directory resolver is unavailable")
	} else {
		sshDirectory, sshErr = application.sshDirectory()
	}
	results := make([]doctorResult, 0, 7)
	results = append(results, checkDoctorLocalState(configDirectory, configErr))

	var client lifecycleClient
	var authOK bool
	if configErr != nil || strings.TrimSpace(application.clientID) == "" || application.newRefreshClient == nil || application.now == nil {
		results = append(results, doctorResult{"authentication", doctorFail, "login configuration is unavailable; set DEVM_WORKOS_CLIENT_ID and run `devm login`"})
	} else {
		var err error
		refresher, err := application.newRefreshClient(application.clientID)
		if err == nil {
			var expiresAt time.Time
			var refreshed bool
			var token string
			if _, urlErr := secureControlPlaneURL(application.controlPlaneURL); urlErr != nil {
				err = urlErr
			} else {
				token, expiresAt, refreshed, err = newTokenSession(configDirectory, refresher, application.now).freshAccessToken(ctx)
			}
			if err == nil {
				api, apiErr := contracts.NewClientWithResponses(application.controlPlaneURL, contracts.WithHTTPClient(cloneProxyHTTPClient(application.httpClient)))
				if apiErr != nil {
					err = apiErr
				} else {
					client = lifecycleClient{api: api, token: token}
				}
			}
			if err == nil {
				authOK = true
				detail := "access token valid until " + expiresAt.UTC().Format(time.RFC3339) + "; refresh not exercised"
				if refreshed {
					detail = "safe refresh exercised successfully; access token valid until " + expiresAt.UTC().Format(time.RFC3339)
				}
				results = append(results, doctorResult{"authentication", doctorPass, detail})
			}
		}
		if err != nil {
			results = append(results, doctorResult{"authentication", doctorFail, "session is missing or cannot be refreshed; run `devm login`"})
		}
	}
	results = append(results, checkDoctorSSHInclude(configDirectory, sshDirectory, configErr, sshErr))
	results = append(results, checkDoctorSSHKey(sshDirectory, sshErr))

	var environments []contracts.Environment
	controlOK := false
	if !authOK {
		results = append(results, doctorResult{"control-plane", doctorFail, "authentication is required before GET /v1/me can be checked"})
	} else {
		user, err := doctorCurrentUser(ctx, client)
		if err != nil {
			results = append(results, doctorResult{"control-plane", doctorFail, "GET /v1/me failed; verify DEVM_CONTROL_PLANE_URL and network access"})
		} else {
			controlOK = true
			results = append(results, doctorResult{"control-plane", doctorPass, "reachable as " + user.Id})
			environments, err = listSetupEnvironments(ctx, client.api, client.token)
			if err != nil {
				controlOK = false
			}
		}
	}
	proxy, guest := doctorEnvironmentReadModels(environments, controlOK)
	results = append(results, proxy, guest)

	failed := false
	for _, result := range results {
		if result.level == doctorFail {
			failed = true
		}
		if _, err := fmt.Fprintf(writerOrDiscard(application.output), "%s\t%s\t%s\n", result.level, result.name, result.detail); err != nil {
			return errors.New("write doctor result")
		}
	}
	if failed {
		return errors.New("doctor found failing checks")
	}
	return nil
}

func checkDoctorLocalState(directory string, resolveErr error) doctorResult {
	if resolveErr != nil || directory == "" {
		return doctorResult{"local-state", doctorFail, "cannot resolve ~/.config/devm; verify the user config directory"}
	}
	state, err := openAnchoredDirectory(directory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return doctorResult{"local-state", doctorWarn, "directory does not exist yet; run `devm login`"}
	}
	if err != nil {
		return doctorResult{"local-state", doctorFail, "directory path is unsafe; remove symlinks and use mode 0700"}
	}
	defer state.Close()
	if err := requirePrivateDirectory(state, "local state"); err != nil {
		return doctorResult{"local-state", doctorFail, "directory must be mode 0700"}
	}
	if info, err := state.root.Lstat("state.lock"); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return doctorResult{"local-state", doctorFail, "state.lock is unsafe; repair it with mode 0600"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return doctorResult{"local-state", doctorFail, "state.lock cannot be inspected safely"}
	}
	configInfo, err := state.root.Lstat("config.toml")
	configMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !configMissing {
		return doctorResult{"local-state", doctorFail, "config.toml cannot be inspected safely"}
	}
	if !configMissing && (!configInfo.Mode().IsRegular() || configInfo.Mode().Perm() != 0o600) {
		return doctorResult{"local-state", doctorFail, "config.toml is unsafe; repair it with mode 0600"}
	}
	if !configMissing {
		if _, err := newLocalStateStore(directory).ReadConfig(); err != nil {
			return doctorResult{"local-state", doctorFail, "config.toml is unsafe or malformed; repair it with mode 0600"}
		}
	}
	if err := checkDoctorProjectBindings(state); err != nil {
		return doctorResult{"local-state", doctorFail, err.Error()}
	}
	if configMissing {
		return doctorResult{"local-state", doctorWarn, "config.toml is absent; defaults will be created on first use"}
	}
	return doctorResult{"local-state", doctorPass, "private directory, config.toml, and Project Bindings are valid"}
}

var projectBindingFilePattern = regexp.MustCompile(`^[0-9a-f]{64}\.toml$`)

func checkDoctorProjectBindings(state *anchoredDirectory) error {
	projects, err := openAnchoredChild(state, "projects", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("projects directory is unsafe; remove symlinks and use mode 0700")
	}
	defer projects.Close()
	handle, err := projects.root.Open(".")
	if err != nil {
		return errors.New("projects directory cannot be read")
	}
	entries, err := handle.ReadDir(-1)
	handle.Close()
	if err != nil {
		return errors.New("projects directory cannot be read")
	}
	for _, entry := range entries {
		if entry.Name() == "bindings.lock" {
			info, statErr := projects.root.Lstat(entry.Name())
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				return errors.New("bindings.lock is unsafe; repair it with mode 0600")
			}
			continue
		}
		if !projectBindingFilePattern.MatchString(entry.Name()) {
			return errors.New("projects directory contains an unexpected entry")
		}
		content, info, readErr := projects.readRegular(entry.Name(), maxLocalStateFileSize)
		if readErr != nil || info.Mode().Perm() != 0o600 {
			return errors.New("a Project Binding is unsafe; repair it with mode 0600")
		}
		var binding projectBinding
		decoder := toml.NewDecoder(bytes.NewReader(content))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&binding) != nil || !validProjectBinding(binding, binding.RepositoryIdentity) || projectBindingName(binding.RepositoryIdentity) != entry.Name() {
			return errors.New("a Project Binding is malformed or stored under the wrong identity")
		}
	}
	return nil
}

func checkDoctorManagedSSHConfig(configDirectory string) error {
	state, err := openAnchoredDirectory(configDirectory, false, 0)
	if err != nil {
		return err
	}
	defer state.Close()
	managed, err := openAnchoredChild(state, "ssh", false)
	if err != nil {
		return err
	}
	defer managed.Close()
	_, info, err := managed.readRegular("config", maxUserSSHConfigSize)
	if err != nil || info.Mode().Perm() != 0o600 {
		return errors.New("managed SSH config is absent or unsafe")
	}
	return nil
}

func checkDoctorSSHInclude(configDirectory, sshDirectory string, configErr, sshErr error) doctorResult {
	if configErr != nil || sshErr != nil || configDirectory == "" || sshDirectory == "" {
		return doctorResult{"ssh-include", doctorFail, "cannot resolve SSH configuration paths"}
	}
	directory, err := openAnchoredDirectory(sshDirectory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return doctorResult{"ssh-include", doctorFail, "~/.ssh is absent; run `devm ssh setup`"}
	}
	if err != nil {
		return doctorResult{"ssh-include", doctorFail, "~/.ssh path is unsafe"}
	}
	defer directory.Close()
	content, _, err := directory.readRegular("config", maxUserSSHConfigSize)
	if errors.Is(err, os.ErrNotExist) {
		return doctorResult{"ssh-include", doctorFail, "SSH config is absent; run `devm ssh setup`"}
	}
	if err != nil {
		return doctorResult{"ssh-include", doctorFail, "SSH config is not a bounded regular file"}
	}
	argument, err := sshConfigArgument(filepath.Join(configDirectory, "ssh", "config"))
	if err != nil {
		return doctorResult{"ssh-include", doctorFail, "managed SSH config path is invalid"}
	}
	want := "Include " + argument
	for _, line := range bytes.Split(content, []byte("\n")) {
		if strings.TrimSpace(string(line)) == want {
			if err := checkDoctorManagedSSHConfig(configDirectory); err != nil {
				return doctorResult{"ssh-include", doctorFail, "managed Include target is absent or unsafe; run `devm ssh setup`"}
			}
			return doctorResult{"ssh-include", doctorPass, "primary SSH config includes devm's managed config"}
		}
	}
	return doctorResult{"ssh-include", doctorFail, "managed Include is missing; run `devm ssh setup`"}
}

func checkDoctorSSHKey(sshDirectory string, resolveErr error) doctorResult {
	if resolveErr != nil || sshDirectory == "" {
		return doctorResult{"ssh-key", doctorFail, "cannot resolve ~/.ssh"}
	}
	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		return doctorResult{"ssh-key", doctorFail, "SSH key directory is unsafe or unreadable"}
	}
	if len(keys) == 0 {
		return doctorResult{"ssh-key", doctorFail, "no usable Ed25519 key pair found; run `devm ssh setup`"}
	}
	directory, err := openAnchoredDirectory(sshDirectory, false, 0)
	if err != nil {
		return doctorResult{"ssh-key", doctorFail, "SSH key directory is unsafe or unreadable"}
	}
	defer directory.Close()
	privateKeys := 0
	for _, key := range keys {
		info, err := directory.root.Lstat(filepath.Base(key.PrivateKeyPath))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 {
			privateKeys++
		}
	}
	if privateKeys == 0 {
		return doctorResult{"ssh-key", doctorFail, "Ed25519 private-key permissions are unsafe; remove group/other access"}
	}
	return doctorResult{"ssh-key", doctorPass, fmt.Sprintf("%d usable private Ed25519 key pair(s) found", privateKeys)}
}

func doctorCurrentUser(ctx context.Context, client lifecycleClient) (contracts.User, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	defer cancel()
	response, err := client.api.GetCurrentUserWithResponse(requestContext, client.editor())
	if err != nil {
		return contracts.User{}, err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id == "" {
		return contracts.User{}, errors.New("invalid current User response")
	}
	return *response.JSON200, nil
}

func doctorEnvironmentReadModels(environments []contracts.Environment, available bool) (doctorResult, doctorResult) {
	if !available {
		detail := "control-plane-derived observation unavailable; restore control-plane access"
		return doctorResult{"proxy-observation", doctorFail, detail}, doctorResult{"guest-observation", doctorFail, detail}
	}
	if environments == nil {
		detail := "control-plane-derived observation unavailable because GET /v1/environments failed"
		return doctorResult{"proxy-observation", doctorFail, detail}, doctorResult{"guest-observation", doctorFail, detail}
	}
	if len(environments) == 0 {
		return doctorResult{"proxy-observation", doctorWarn, "control-plane-derived only; no Environment exists and the proxy was not directly probed"},
			doctorResult{"guest-observation", doctorWarn, "control-plane-derived only; no Runtime exists and the guest was not directly probed"}
	}
	activeOperations, ready, transitioning, runtimeErrors, degraded, blocked, unknown := 0, 0, 0, 0, 0, 0, 0
	for _, environment := range environments {
		if environment.ActiveOperationId != nil && *environment.ActiveOperationId != "" {
			activeOperations++
		}
		if environment.Runtime == nil {
			// Health is still authoritative even when no Runtime projection exists.
		} else {
			switch environment.Runtime.Status {
			case contracts.RuntimeStatusReady:
				ready++
			case contracts.RuntimeStatusError:
				runtimeErrors++
			case contracts.RuntimeStatusProvisioning, contracts.RuntimeStatusStarting,
				contracts.RuntimeStatusStopping, contracts.RuntimeStatusReplacing:
				transitioning++
			}
		}
		switch environment.Health {
		case contracts.EnvironmentHealthBlocked:
			blocked++
		case contracts.EnvironmentHealthDegraded:
			degraded++
		case contracts.EnvironmentHealthUnknown:
			unknown++
		case contracts.EnvironmentHealthHealthy:
		default:
			unknown++
		}
	}
	proxy := doctorResult{"proxy-observation", doctorPass, fmt.Sprintf("control-plane-derived only; proxy not directly probed; %d active Operation(s)", activeOperations)}
	if activeOperations > 0 || transitioning > 0 {
		proxy.level = doctorWarn
		proxy.detail += "; wait for active transitions before connecting"
	}
	if runtimeErrors > 0 {
		proxy.level = doctorFail
		proxy.detail = fmt.Sprintf("%d Runtime(s) report error; inspect `devm status`", runtimeErrors)
	}
	guest := doctorResult{"guest-observation", doctorWarn, "control-plane-derived only; guest not directly probed; no Runtime reports ready"}
	if ready > 0 {
		guest = doctorResult{"guest-observation", doctorPass, fmt.Sprintf("control-plane-derived only; guest not directly probed; %d Runtime(s) report ready", ready)}
	}
	if runtimeErrors > 0 {
		guest = doctorResult{"guest-state", doctorFail, fmt.Sprintf("%d Runtime(s) report error; inspect `devm status`", runtimeErrors)}
	} else if transitioning > 0 && ready == 0 {
		guest = doctorResult{"guest-observation", doctorWarn, fmt.Sprintf("control-plane-derived only; %d Runtime(s) are transitioning", transitioning)}
	}
	if blocked > 0 {
		proxy = doctorResult{"proxy-observation", doctorFail, fmt.Sprintf("control-plane-derived: %d Environment(s) are blocked; proxy not directly probed", blocked)}
		guest = doctorResult{"guest-observation", doctorFail, fmt.Sprintf("control-plane-derived: %d Environment(s) are blocked; guest not directly probed", blocked)}
	} else if runtimeErrors > 0 {
		proxy = doctorResult{"proxy-observation", doctorFail, fmt.Sprintf("control-plane-derived: %d Runtime(s) report error; proxy not directly probed; inspect `devm status`", runtimeErrors)}
		guest = doctorResult{"guest-observation", doctorFail, fmt.Sprintf("control-plane-derived: %d Runtime(s) report error; guest not directly probed; inspect `devm status`", runtimeErrors)}
	} else if degraded > 0 || unknown > 0 {
		detail := fmt.Sprintf("control-plane-derived: %d degraded and %d unknown Environment(s); not directly probed", degraded, unknown)
		proxy = doctorResult{"proxy-observation", doctorWarn, detail}
		guest = doctorResult{"guest-observation", doctorWarn, detail}
	}
	return proxy, guest
}
