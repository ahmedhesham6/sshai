package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	return newCLI().run(ctx, arguments)
}

type cli struct {
	output           io.Writer
	errorOutput      io.Writer
	input            io.Reader
	clientID         string
	controlPlaneURL  string
	httpClient       *http.Client
	now              func() time.Time
	workingDirectory func() (string, error)
	configDirectory  func() (string, error)
	sshDirectory     func() (string, error)
	newLoginFlow     func(string) (loginFlow, error)
	newRefreshClient func(string) (tokenRefresher, error)
	newAttempt       func() (string, error)
}

func newCLI() cli {
	return cli{
		output:           os.Stdout,
		errorOutput:      os.Stderr,
		input:            os.Stdin,
		clientID:         os.Getenv("DEVM_WORKOS_CLIENT_ID"),
		controlPlaneURL:  os.Getenv("DEVM_CONTROL_PLANE_URL"),
		httpClient:       http.DefaultClient,
		now:              time.Now,
		workingDirectory: os.Getwd,
		configDirectory: func() (string, error) {
			root, err := os.UserConfigDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(root, "devm"), nil
		},
		sshDirectory: func() (string, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(home, ".ssh"), nil
		},
		newLoginFlow: newWorkOSLoginFlow,
		newRefreshClient: func(clientID string) (tokenRefresher, error) {
			return auth.NewRefreshClient(clientID)
		},
		newAttempt: newCLIAttempt,
	}
}

func (application cli) run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return fmt.Errorf("usage: devm <inspect|plan|capsule|login|ssh|ssh-proxy>")
	}
	switch arguments[0] {
	case "login":
		if len(arguments) != 1 {
			return fmt.Errorf("usage: devm login")
		}
		if application.clientID == "" {
			return fmt.Errorf("DEVM_WORKOS_CLIENT_ID is required")
		}
		flow, err := application.newLoginFlow(application.clientID)
		if err != nil {
			return err
		}
		configDirectory, err := application.configDirectory()
		if err != nil {
			return fmt.Errorf("resolve user config directory: %w", err)
		}
		return runLogin(ctx, flow, configDirectory, application.output)
	case "plan":
		return application.runPlan(ctx, arguments[1:])
	case "inspect":
		return application.runInspect(ctx, arguments[1:])
	case "capsule":
		return application.runCapsule(ctx, arguments[1:])
	case "ssh":
		return application.runSSH(ctx, arguments[1:])
	case "ssh-proxy":
		return application.runSSHProxy(ctx, arguments[1:])
	default:
		return fmt.Errorf("usage: devm <inspect|plan|capsule|login|ssh|ssh-proxy>")
	}
}

func (application cli) runSSH(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "setup" {
		return errors.New("usage: devm ssh setup [--identity-file PATH]")
	}
	flags := flag.NewFlagSet("ssh setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	identityFile := flags.String("identity-file", "", "discovered Ed25519 private-key path")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("usage: devm ssh setup [--identity-file PATH]")
	}
	if strings.TrimSpace(application.clientID) == "" {
		return errors.New("DEVM_WORKOS_CLIENT_ID is required")
	}
	if _, err := secureControlPlaneURL(application.controlPlaneURL); err != nil {
		return err
	}
	if application.newRefreshClient == nil || application.configDirectory == nil || application.sshDirectory == nil {
		return errors.New("configure SSH setup: command is incomplete")
	}
	refresher, err := application.newRefreshClient(application.clientID)
	if err != nil {
		return err
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return errors.New("resolve user config directory for SSH setup")
	}
	sshDirectory, err := application.sshDirectory()
	if err != nil {
		return errors.New("resolve user SSH directory for SSH setup")
	}
	command := sshSetupCommand{
		controlPlaneURL: application.controlPlaneURL,
		httpClient:      application.httpClient,
		tokens:          newTokenSession(configDirectory, refresher, application.now),
		configDirectory: configDirectory,
		sshDirectory:    sshDirectory,
		output:          application.output,
	}
	return command.run(ctx, *identityFile)
}

func (application cli) runCapsule(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: devm capsule <capture|build> [flags]")
	}
	command := arguments[0]
	flags := flag.NewFlagSet("capsule "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profileRoot := flags.String("profile-root", ".", "root containing Capsule candidates")
	selectionsFile := flags.String("selections", "", "JSON file containing PATH/SELECTOR selections")
	var selections selectorFlags
	flags.Var(&selections, "select", "explicit Profile path and selector")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("usage: devm capsule %s --profile-root PATH [--select PATH=SELECTOR] [--selections FILE]", command)
	}
	resolvedSelections, err := loadSelections(*selectionsFile, selections)
	if err != nil {
		return err
	}
	switch command {
	case "capture":
		return RunCapsuleCapture(ctx, *profileRoot, resolvedSelections, application.output)
	case "build":
		return RunCapsuleBuild(ctx, *profileRoot, resolvedSelections, application.output)
	default:
		return errors.New("usage: devm capsule <capture|build> [flags]")
	}
}

func (application cli) runSSHProxy(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("ssh-proxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	environmentID := flags.String("environment", "", "stable Environment identifier")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *environmentID == "" {
		return errors.New("usage: devm ssh-proxy --environment ID")
	}
	if strings.TrimSpace(application.clientID) == "" {
		return errors.New("DEVM_WORKOS_CLIENT_ID is required")
	}
	if _, err := secureControlPlaneURL(application.controlPlaneURL); err != nil {
		return err
	}
	if application.newRefreshClient == nil || application.newAttempt == nil || application.configDirectory == nil {
		return errors.New("configure SSH proxy: command is incomplete")
	}
	refresher, err := application.newRefreshClient(application.clientID)
	if err != nil {
		return err
	}
	configDirectory, err := application.configDirectory()
	if err != nil {
		return errors.New("resolve user config directory for SSH proxy")
	}
	attempt, err := application.newAttempt()
	if err != nil {
		return errors.New("create SSH proxy attempt")
	}
	command := sshProxyCommand{
		controlPlaneURL: application.controlPlaneURL,
		httpClient:      application.httpClient,
		tokens:          newTokenSession(configDirectory, refresher, application.now),
		attempt:         attempt,
		input:           application.input,
		output:          application.output,
		errorOutput:     application.errorOutput,
		now:             application.now,
	}
	return command.run(ctx, *environmentID)
}

func newCLIAttempt() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy[:]), nil
}

func (application cli) runInspect(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profileRoot := flags.String("profile-root", ".", "root containing Profile candidates")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: devm inspect [--profile-root PATH]")
	}
	repositoryRoot, err := application.workingDirectory()
	if err != nil {
		return fmt.Errorf("resolve repository directory: %w", err)
	}
	sshDirectory, err := application.sshDirectory()
	if err != nil {
		return fmt.Errorf("resolve SSH directory: %w", err)
	}
	return RunInspect(ctx, repositoryRoot, *profileRoot, sshDirectory, application.output)
}

func (application cli) runPlan(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profileRoot := flags.String("profile-root", ".", "root containing selected Profile content")
	var selections selectorFlags
	flags.Var(&selections, "select", "explicit Profile path and selector")
	selectionsFile := flags.String("selections", "", "JSON file containing PATH/SELECTOR selections")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	repositoryRoot, err := application.workingDirectory()
	if err != nil {
		return fmt.Errorf("resolve repository directory: %w", err)
	}
	resolvedSelections, err := loadSelections(*selectionsFile, selections)
	if err != nil {
		return err
	}
	return RunPlan(ctx, repositoryRoot, *profileRoot, resolvedSelections, application.output)
}

type selectorFlags []profile.Selector

func (values *selectorFlags) String() string { return "" }

func (values *selectorFlags) Set(value string) error {
	path, selector, ok := strings.Cut(value, "=")
	if !ok || path == "" || selector == "" {
		return fmt.Errorf("selection must be PATH=SELECTOR")
	}
	*values = append(*values, profile.Selector{Path: path, Selector: selector})
	return nil
}

func loadSelections(path string, flags selectorFlags) ([]profile.Selector, error) {
	selections := append([]profile.Selector(nil), flags...)
	if path == "" {
		return selections, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read selections file: %w", err)
	}
	var fileSelections []profile.Selector
	if err := json.Unmarshal(content, &fileSelections); err != nil {
		return nil, fmt.Errorf("decode selections file: %w", err)
	}
	return append(selections, fileSelections...), nil
}
