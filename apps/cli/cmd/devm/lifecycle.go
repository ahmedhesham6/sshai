package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const lifecycleRequestTimeout = 15 * time.Second

type sshClientRunner func(context.Context, string, io.Reader, io.Writer, io.Writer) error

func runOpenSSH(ctx context.Context, alias string, input io.Reader, output, errorOutput io.Writer) error {
	command := exec.CommandContext(ctx, "ssh", alias)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if err := command.Run(); err != nil {
		return fmt.Errorf("connect to Environment with ssh: %w", err)
	}
	return nil
}

type lifecycleClient struct {
	api   *contracts.ClientWithResponses
	token string
}

func (application cli) lifecycleClient(ctx context.Context) (lifecycleClient, error) {
	if strings.TrimSpace(application.clientID) == "" {
		return lifecycleClient{}, errors.New("not authenticated: set DEVM_WORKOS_CLIENT_ID and run `devm login`")
	}
	if _, err := secureControlPlaneURL(application.controlPlaneURL); err != nil {
		return lifecycleClient{}, err
	}
	if application.newRefreshClient == nil || application.configDirectory == nil {
		return lifecycleClient{}, errors.New("configure lifecycle command: command is incomplete")
	}
	refresher, err := application.newRefreshClient(application.clientID)
	if err != nil {
		return lifecycleClient{}, errors.New("authenticate: initialize token refresh")
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return lifecycleClient{}, errors.New("authenticate: resolve local state directory")
	}
	token, err := newTokenSession(configDirectory, refresher, application.now).FreshAccessToken(ctx)
	if err != nil || token == "" {
		return lifecycleClient{}, errors.New("not authenticated: run `devm login`")
	}
	api, err := contracts.NewClientWithResponses(application.controlPlaneURL, contracts.WithHTTPClient(cloneProxyHTTPClient(application.httpClient)))
	if err != nil {
		return lifecycleClient{}, errors.New("configure lifecycle command: control plane URL is invalid")
	}
	return lifecycleClient{api: api, token: token}, nil
}

func (client lifecycleClient) editor() contracts.RequestEditorFn {
	return bearerRequestEditor(client.token)
}

func (application cli) runBare(ctx context.Context) error {
	workingDirectory, err := application.workingDirectory()
	if err != nil {
		return errors.New("resolve repository directory")
	}
	identity, root, err := canonicalRepositoryIdentity(ctx, workingDirectory, application.git)
	if err != nil {
		return err
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return errors.New("resolve local state directory")
	}
	store := newLocalStateStore(configDirectory)
	binding, bound, err := store.ReadProject(identity)
	if err != nil {
		return err
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	var environment contracts.Environment
	if bound {
		environment, err = getLifecycleEnvironment(ctx, client, binding.EnvironmentID)
		if err != nil {
			return fmt.Errorf("resolve bound Environment: %w", err)
		}
	} else {
		if err := store.EnsureConfig(ctx); err != nil {
			return err
		}
		config, err := store.ReadConfig()
		if err != nil {
			return err
		}
		if config.ProfileVersionID == "" || config.ProjectSeedID == "" || len(config.SSHKeyIDs) == 0 {
			return errors.New("create Environment: config.toml must set profile_version_id, project_seed_id, and at least one ssh_key_ids entry")
		}
		body := contracts.CreateEnvironmentJSONRequestBody{
			Name: repositoryEnvironmentName(root), Region: config.DefaultRegion, RuntimePreset: config.RuntimePreset,
			ProfileVersionId: config.ProfileVersionID, ProjectSeedId: config.ProjectSeedID,
			SshKeyIds: append([]string(nil), config.SSHKeyIDs...),
			AutoStopPolicy: contracts.AutoStopPolicy{
				Mode: contracts.AutoStopPolicyMode(config.AutoStopMode), GracePeriodSeconds: config.AutoStopGracePeriodSecs,
			},
		}
		idempotencyKey := deterministicKey("environment-create", identity)
		requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
		response, requestErr := client.api.CreateEnvironmentWithResponse(requestContext,
			&contracts.CreateEnvironmentParams{IdempotencyKey: idempotencyKey}, body, client.editor())
		cancel()
		if requestErr != nil {
			return lifecycleUnavailable(ctx, "create Environment", requestErr)
		}
		if response.StatusCode() != http.StatusAccepted || response.JSON202 == nil || response.JSON202.Environment.Id == "" {
			return fmt.Errorf("create Environment: control plane returned HTTP %d", response.StatusCode())
		}
		environment = response.JSON202.Environment
		if err := store.BindProject(ctx, identity, environment.Id); err != nil {
			return fmt.Errorf("save Project Binding: %w", err)
		}
	}
	if environment.Lifecycle == contracts.Deleted || environment.Id == "" || !sshIdentifierPattern.MatchString(environment.Slug) {
		return errors.New("connect to Environment: control plane returned invalid Environment identity")
	}
	if err := application.ensureSSHSetup(ctx); err != nil {
		return err
	}
	if application.runSSHClient == nil {
		return errors.New("connect to Environment: SSH client is unavailable")
	}
	return application.runSSHClient(ctx, environment.Slug, application.input, application.output, application.errorOutput)
}

func (application cli) ensureSSHSetup(ctx context.Context) error {
	refresher, err := application.newRefreshClient(application.clientID)
	if err != nil {
		return errors.New("configure SSH access: initialize token refresh")
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return errors.New("configure SSH access: resolve local state directory")
	}
	sshDirectory, err := application.sshDirectory()
	if err != nil {
		return errors.New("configure SSH access: resolve SSH directory")
	}
	command := sshSetupCommand{
		controlPlaneURL: application.controlPlaneURL, httpClient: application.httpClient,
		tokens:          newTokenSession(configDirectory, refresher, application.now),
		configDirectory: configDirectory, sshDirectory: sshDirectory, output: application.output,
	}
	if err := command.run(ctx, ""); err != nil {
		return fmt.Errorf("configure SSH access: %w", err)
	}
	return nil
}

func repositoryEnvironmentName(root string) string {
	name := strings.ToLower(filepath.Base(root))
	name = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) < 3 {
		name = "dev-" + name
	}
	for utf8.RuneCountInString(name) > 64 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return strings.TrimRight(name, "-")
}

func deterministicKey(action, identity string) string {
	digest := sha256.Sum256([]byte(action + "\x00" + identity))
	return action + "-" + hex.EncodeToString(digest[:])[:32]
}

func getLifecycleEnvironment(ctx context.Context, client lifecycleClient, environmentID string) (contracts.Environment, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetEnvironmentWithResponse(requestContext, environmentID, client.editor())
	cancel()
	if err != nil {
		return contracts.Environment{}, lifecycleUnavailable(ctx, "get Environment", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
		return contracts.Environment{}, fmt.Errorf("control plane returned HTTP %d", response.StatusCode())
	}
	if response.JSON200.Id != environmentID {
		return contracts.Environment{}, errors.New("control plane returned a mismatched Environment")
	}
	return *response.JSON200, nil
}

func lifecycleUnavailable(ctx context.Context, action string, _ error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return errors.New(action + ": control plane is unavailable")
}

func (application cli) resolveEnvironmentID(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	workingDirectory, err := application.workingDirectory()
	if err != nil {
		return "", errors.New("resolve repository directory")
	}
	identity, _, err := canonicalRepositoryIdentity(ctx, workingDirectory, application.git)
	if err != nil {
		return "", err
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return "", errors.New("resolve local state directory")
	}
	binding, found, err := newLocalStateStore(configDirectory).ReadProject(identity)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("no Project Binding found; run `devm` first or pass --environment ID")
	}
	return binding.EnvironmentID, nil
}

type statusResult struct {
	Environment contracts.Environment    `json:"environment"`
	Operation   *contracts.Operation     `json:"operation"`
	Billing     contracts.BillingSummary `json:"billing"`
}

func (application cli) runStatus(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	environmentFlag := flags.String("environment", "", "Environment ID")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("usage: devm status [--environment ID] [--json]")
	}
	environmentID, err := application.resolveEnvironmentID(ctx, *environmentFlag)
	if err != nil {
		return err
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	environment, err := getLifecycleEnvironment(ctx, client, environmentID)
	if err != nil {
		return fmt.Errorf("show status: %w", err)
	}
	var operation *contracts.Operation
	if environment.ActiveOperationId != nil && *environment.ActiveOperationId != "" {
		operation, err = getLifecycleOperation(ctx, client, *environment.ActiveOperationId)
		if err != nil {
			return fmt.Errorf("show status: %w", err)
		}
	}
	billing, err := getLifecycleBilling(ctx, client)
	if err != nil {
		return fmt.Errorf("show status: %w", err)
	}
	result := statusResult{Environment: environment, Operation: operation, Billing: billing}
	if *jsonOutput {
		encoder := json.NewEncoder(writerOrDiscard(application.output))
		encoder.SetEscapeHTML(true)
		if err := encoder.Encode(result); err != nil {
			return errors.New("write status JSON")
		}
		return nil
	}
	runtimeStatus := contracts.RuntimeStatusAbsent
	if environment.Runtime != nil {
		runtimeStatus = environment.Runtime.Status
	}
	activeOperation := "none"
	if operation != nil {
		activeOperation = operation.Type + " (" + string(operation.Status) + ")"
	}
	output := writerOrDiscard(application.output)
	_, err = fmt.Fprintf(output,
		"FIELD\tVALUE\nEnvironment\t%s (%s)\nRuntime\t%s\nAuto-stop\t%s, %ds grace\nActive operation\t%s\nCredits\t%d (%s)\n",
		environment.Name, environment.Id, runtimeStatus, environment.AutoStopPolicy.Mode,
		environment.AutoStopPolicy.GracePeriodSeconds, activeOperation, billing.CreditBalance, billing.SubscriptionStatus)
	if err != nil {
		return errors.New("write status")
	}
	return nil
}

func getLifecycleOperation(ctx context.Context, client lifecycleClient, operationID string) (*contracts.Operation, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetOperationWithResponse(requestContext, operationID, client.editor())
	cancel()
	if err != nil {
		return nil, lifecycleUnavailable(ctx, "get Operation", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != operationID {
		return nil, fmt.Errorf("get Operation: control plane returned HTTP %d", response.StatusCode())
	}
	return response.JSON200, nil
}

func getLifecycleBilling(ctx context.Context, client lifecycleClient) (contracts.BillingSummary, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetBillingSummaryWithResponse(requestContext, client.editor())
	cancel()
	if err != nil {
		return contracts.BillingSummary{}, lifecycleUnavailable(ctx, "get billing", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
		return contracts.BillingSummary{}, fmt.Errorf("get billing: control plane returned HTTP %d", response.StatusCode())
	}
	return *response.JSON200, nil
}

func (application cli) runStop(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	environmentFlag := flags.String("environment", "", "Environment ID")
	noWait := flags.Bool("no-wait", false, "do not poll the Operation")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("usage: devm stop [--environment ID] [--no-wait]")
	}
	environmentID, err := application.resolveEnvironmentID(ctx, *environmentFlag)
	if err != nil {
		return err
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	reason := contracts.StopEnvironmentRuntimeJSONBodyReasonManual
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.StopEnvironmentRuntimeWithResponse(requestContext, environmentID,
		&contracts.StopEnvironmentRuntimeParams{IdempotencyKey: deterministicKey("environment-stop", environmentID)},
		contracts.StopEnvironmentRuntimeJSONRequestBody{Reason: &reason}, client.editor())
	cancel()
	if requestErr != nil {
		return lifecycleUnavailable(ctx, "stop Runtime", requestErr)
	}
	if response.StatusCode() != http.StatusAccepted || response.JSON202 == nil || response.JSON202.Operation.Id == "" {
		return fmt.Errorf("stop Runtime: control plane returned HTTP %d", response.StatusCode())
	}
	operation := response.JSON202.Operation
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Stop requested: %s (%s).\n", operation.Id, operation.Status); err != nil {
		return errors.New("write stop result")
	}
	if *noWait {
		return nil
	}
	return application.pollStopOperation(ctx, client, operation)
}

func (application cli) pollStopOperation(ctx context.Context, client lifecycleClient, operation contracts.Operation) error {
	interval := application.stopPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := application.stopWaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	wait := application.wait
	if wait == nil {
		wait = waitForContext
	}
	for !operationTerminal(operation.Status) {
		if err := wait(waitContext, interval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("stop Runtime: Operation %s is still %s after %s; check `devm status`", operation.Id, operation.Status, timeout)
			}
			return err
		}
		current, err := getLifecycleOperation(waitContext, client, operation.Id)
		if err != nil {
			return fmt.Errorf("poll stop Operation: %w", err)
		}
		operation = *current
	}
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Stop Operation %s: %s.\n", operation.Id, operation.Status); err != nil {
		return errors.New("write stop result")
	}
	if operation.Status != contracts.OperationStatusSucceeded {
		return fmt.Errorf("stop Runtime: Operation %s ended %s; persistent Environment state remains intact", operation.Id, operation.Status)
	}
	return nil
}

func operationTerminal(status contracts.OperationStatus) bool {
	switch status {
	case contracts.OperationStatusSucceeded, contracts.OperationStatusFailed,
		contracts.OperationStatusCancelled, contracts.OperationStatusBlocked:
		return true
	default:
		return false
	}
}

func waitForContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}
