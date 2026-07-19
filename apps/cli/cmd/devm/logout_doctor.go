package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/contracts"
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
	if configErr != nil || strings.TrimSpace(application.clientID) == "" || application.newRefreshClient == nil {
		results = append(results, doctorResult{"authentication", doctorFail, "login configuration is unavailable; set DEVM_WORKOS_CLIENT_ID and run `devm login`"})
	} else {
		var err error
		client, err = application.lifecycleClient(ctx)
		if err != nil {
			results = append(results, doctorResult{"authentication", doctorFail, "session is missing or cannot be refreshed; run `devm login`"})
		} else {
			authOK = true
			results = append(results, doctorResult{"authentication", doctorPass, "local session is present and refreshable"})
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
	if _, err := newLocalStateStore(directory).ReadConfig(); err != nil {
		return doctorResult{"local-state", doctorFail, "config.toml is unsafe or malformed; repair it with mode 0600"}
	}
	return doctorResult{"local-state", doctorPass, "private directory and config.toml are valid"}
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
		detail := "Environment read models are unavailable; restore control-plane access"
		return doctorResult{"proxy-state", doctorFail, detail}, doctorResult{"guest-state", doctorFail, detail}
	}
	if environments == nil {
		detail := "GET /v1/environments failed; retry after control-plane recovery"
		return doctorResult{"proxy-state", doctorFail, detail}, doctorResult{"guest-state", doctorFail, detail}
	}
	if len(environments) == 0 {
		return doctorResult{"proxy-state", doctorWarn, "no Environment read models to inspect"},
			doctorResult{"guest-state", doctorWarn, "no Runtime readiness state to inspect"}
	}
	activeOperations, ready, transitioning, runtimeErrors := 0, 0, 0, 0
	for _, environment := range environments {
		if environment.ActiveOperationId != nil && *environment.ActiveOperationId != "" {
			activeOperations++
		}
		if environment.Runtime == nil {
			continue
		}
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
	proxy := doctorResult{"proxy-state", doctorPass, fmt.Sprintf("read models reachable; %d active Operation(s)", activeOperations)}
	if activeOperations > 0 || transitioning > 0 {
		proxy.level = doctorWarn
		proxy.detail += "; wait for active transitions before connecting"
	}
	if runtimeErrors > 0 {
		proxy.level = doctorFail
		proxy.detail = fmt.Sprintf("%d Runtime(s) report error; inspect `devm status`", runtimeErrors)
	}
	guest := doctorResult{"guest-state", doctorWarn, "no Runtime currently reports ready; start or connect to an Environment"}
	if ready > 0 {
		guest = doctorResult{"guest-state", doctorPass, fmt.Sprintf("%d Runtime(s) report ready through Environment read models", ready)}
	}
	if runtimeErrors > 0 {
		guest = doctorResult{"guest-state", doctorFail, fmt.Sprintf("%d Runtime(s) report error; inspect `devm status`", runtimeErrors)}
	} else if transitioning > 0 && ready == 0 {
		guest = doctorResult{"guest-state", doctorWarn, fmt.Sprintf("%d Runtime(s) are transitioning; retry after the active Operation", transitioning)}
	}
	return proxy, guest
}
