// Command guest runs the per-Runtime guest supervisor and its private mTLS
// control endpoint.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const (
	maximumRequestDuration      = 10 * time.Minute
	gracefulStopDuration        = 11 * time.Minute
	defaultAgentVersionManifest = "/etc/sshai/agent-versions"
)

func main() {
	// Guest libraries create durable state and key material. Keep the process
	// umask private before configuration or any filesystem operation occurs.
	syscall.Umask(0o077)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("guest supervisor stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	bootID, err := readOneLine(config.bootIDFile)
	if err != nil {
		return fmt.Errorf("read boot ID: %w", err)
	}
	identity := guest.BootIdentity{RuntimeID: config.target.RuntimeID, BootID: bootID, RuntimeSequence: config.runtimeSequence}
	boot := currentBootSource{identity: identity, bootIDFile: config.bootIDFile}
	reporter, _, err := guest.NewReadinessReporter(ctx, identity, boot, time.Now().UTC())
	if err != nil {
		return err
	}
	layout, err := guest.BootstrapPersistentState(ctx, guest.PersistentStateRequest{
		Root: config.dataRoot, ExpectedDeviceID: config.dataDeviceID, ExpectedVolumeID: config.dataVolumeID,
	}, linuxMountInspector{})
	if err != nil {
		return err
	}
	config.workspaceRoot, config.homeRoot, config.cacheRoot, config.platformRoot = layout.Workspace, layout.Home, layout.Cache, layout.Platform
	for _, root := range []string{config.workspaceRoot, config.homeRoot} {
		if err := chownTree(root, config.devUID, config.devGID); err != nil {
			return fmt.Errorf("set dev ownership on %s: %w", root, err)
		}
	}
	if _, err := reporter.Advance(ctx, guest.ReadinessDataMounted, time.Now().UTC()); err != nil {
		return fmt.Errorf("record mounted State Components: %w", err)
	}

	var observer *guest.Observer
	if config.activitySampleFile != "" {
		observer, err = guest.NewObserver(config.target.RuntimeID, &JSONActivitySampleSource{
			Path: config.activitySampleFile, Target: config.target, TrustedUID: os.Geteuid(), MaxAge: 5 * time.Minute, Now: time.Now,
		}, guest.Allowlists{
			CodexExecutables: splitList(config.codexExecutables), ClaudeExecutables: splitList(config.claudeExecutables),
			ProtectedExecutables: splitList(config.protectedExecutables), SelectedContainers: splitList(config.selectedContainers),
		})
		if err != nil {
			return fmt.Errorf("construct Activity Snapshot observer: %w", err)
		}
	}

	agentRequirements, err := readAgentRequirements(config.agentVersionFile)
	if err != nil {
		return fmt.Errorf("read pinned agent versions: %w", err)
	}
	operations, err := guestcontrol.NewLocalOperations(guestcontrol.LocalOperationsConfig{
		Target: config.target, Readiness: reporter,
		WorkspaceRoot: config.workspaceRoot, HomeRoot: config.homeRoot, CacheRoot: config.cacheRoot,
		PlatformRoot: config.platformRoot, SSHDRoot: config.sshdRoot,
		DevUID: config.devUID, DevGID: config.devGID,
		HostIdentityGenerator: ed25519HostIdentityGenerator{},
		HostIdentityActivator: sshdIdentityActivator{
			reloader: systemdSSHReloader{}, prober: tcpSSHHostKeyProber{port: config.sshPort},
		},
		SSHKeys:              optionalSSHKeySource(config.authorizedKeysFile),
		ManagedConfiguration: optionalManagedConfigurationSource(config.managedConfigurationFile),
		AgentRequirements:    agentRequirements,
		Activity:             observer,
		Shutdown: func(context.Context) error {
			syscall.Sync()
			return nil
		},
	})
	if err != nil {
		return err
	}
	if _, err := operations.RestoreSSHHostIdentity(ctx, config.target); err != nil {
		return fmt.Errorf("restore SSH host identity: %w", err)
	}
	handler, err := guestcontrol.NewServer(guestcontrol.ServerConfig{
		Target: config.target, ClientIdentity: config.clientIdentity,
	}, operations)
	if err != nil {
		return err
	}
	tlsConfig, err := guestcontrol.LoadServerTLSConfig(config.certificateFile, config.privateKeyFile, config.clientCAFile)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: config.listenAddress, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: maximumRequestDuration, WriteTimeout: maximumRequestDuration,
		IdleTimeout: 60 * time.Second,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServeTLS("", "") }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve guest control API: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), gracefulStopDuration)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down guest control API: %w", err)
		}
		syscall.Sync()
		if err := <-result; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve guest control API: %w", err)
		}
		return nil
	}
}

type config struct {
	listenAddress   string
	certificateFile string
	privateKeyFile  string
	clientCAFile    string
	clientIdentity  string
	target          guestcontrol.Target
	runtimeSequence int64
	bootIDFile      string
	dataRoot        string
	dataDeviceID    string
	dataVolumeID    string
	workspaceRoot   string
	homeRoot        string
	cacheRoot       string
	platformRoot    string
	sshdRoot        string
	sshPort         int
	devUID          int
	devGID          int

	authorizedKeysFile       string
	managedConfigurationFile string
	activitySampleFile       string
	codexExecutables         string
	claudeExecutables        string
	protectedExecutables     string
	selectedContainers       string
	agentVersionFile         string
}

func loadConfig() (config, error) {
	runtimeSequence, err := strconv.ParseInt(os.Getenv("GUEST_RUNTIME_SEQUENCE"), 10, 64)
	if err != nil || runtimeSequence < 1 {
		return config{}, errors.New("GUEST_RUNTIME_SEQUENCE must be a positive integer")
	}
	dataRoot := valueOrDefault("GUEST_DATA_ROOT", "/var/lib/devm")
	devUID, err := positiveInt("GUEST_DEV_UID")
	if err != nil {
		return config{}, err
	}
	devGID, err := positiveInt("GUEST_DEV_GID")
	if err != nil {
		return config{}, err
	}
	sshPort, err := intOrDefault("GUEST_SSH_PORT", 22)
	if err != nil || sshPort < 1 || sshPort > 65535 {
		return config{}, errors.New("GUEST_SSH_PORT must be between 1 and 65535")
	}
	value := config{
		listenAddress:   os.Getenv("GUEST_LISTEN_ADDR"),
		certificateFile: os.Getenv("GUEST_TLS_CERT_FILE"), privateKeyFile: os.Getenv("GUEST_TLS_KEY_FILE"),
		clientCAFile: os.Getenv("GUEST_TLS_CLIENT_CA_FILE"), clientIdentity: os.Getenv("GUEST_TLS_CLIENT_URI"), runtimeSequence: runtimeSequence,
		bootIDFile: valueOrDefault("GUEST_BOOT_ID_FILE", "/proc/sys/kernel/random/boot_id"),
		dataRoot:   dataRoot, dataDeviceID: os.Getenv("GUEST_DATA_DEVICE_ID"), dataVolumeID: os.Getenv("GUEST_DATA_VOLUME_ID"),
		target: guestcontrol.Target{
			OwnerUserID: os.Getenv("GUEST_OWNER_USER_ID"), EnvironmentID: os.Getenv("GUEST_ENVIRONMENT_ID"),
			RuntimeID: os.Getenv("GUEST_RUNTIME_ID"), ProviderID: os.Getenv("GUEST_PROVIDER_ID"),
			PrivateIPv4: os.Getenv("GUEST_PRIVATE_IPV4"),
		},
		workspaceRoot: filepath.Join(dataRoot, "workspace"), homeRoot: filepath.Join(dataRoot, "home"),
		cacheRoot: filepath.Join(dataRoot, "cache"), platformRoot: filepath.Join(dataRoot, "platform"),
		sshdRoot: valueOrDefault("GUEST_SSHD_ROOT", "/etc/ssh"),
		sshPort:  sshPort, devUID: devUID, devGID: devGID,
		authorizedKeysFile: os.Getenv("GUEST_AUTHORIZED_KEYS_FILE"), managedConfigurationFile: os.Getenv("GUEST_MANAGED_CONFIGURATION_FILE"),
		activitySampleFile: os.Getenv("GUEST_ACTIVITY_SAMPLE_FILE"),
		codexExecutables:   os.Getenv("GUEST_CODEX_EXECUTABLES"), claudeExecutables: os.Getenv("GUEST_CLAUDE_EXECUTABLES"),
		protectedExecutables: os.Getenv("GUEST_PROTECTED_EXECUTABLES"), selectedContainers: os.Getenv("GUEST_SELECTED_CONTAINERS"),
		agentVersionFile: valueOrDefault("GUEST_AGENT_VERSION_FILE", defaultAgentVersionManifest),
	}
	if value.listenAddress == "" {
		value.listenAddress = net.JoinHostPort(value.target.PrivateIPv4, "9443")
	}
	if value.certificateFile == "" || value.privateKeyFile == "" || value.clientCAFile == "" || value.clientIdentity == "" ||
		value.target.OwnerUserID == "" || value.target.EnvironmentID == "" || value.target.RuntimeID == "" || value.target.ProviderID == "" || value.target.PrivateIPv4 == "" ||
		value.dataDeviceID == "" || value.dataVolumeID == "" || value.authorizedKeysFile == "" || value.managedConfigurationFile == "" {
		return config{}, errors.New("GUEST_TLS_CERT_FILE, GUEST_TLS_KEY_FILE, GUEST_TLS_CLIENT_CA_FILE, GUEST_TLS_CLIENT_URI, GUEST_OWNER_USER_ID, GUEST_ENVIRONMENT_ID, GUEST_RUNTIME_ID, GUEST_PROVIDER_ID, GUEST_PRIVATE_IPV4, GUEST_DATA_DEVICE_ID, GUEST_DATA_VOLUME_ID, GUEST_AUTHORIZED_KEYS_FILE, and GUEST_MANAGED_CONFIGURATION_FILE are required")
	}
	if err := validatePrivateListenAddress(value.listenAddress); err != nil {
		return config{}, err
	}
	privateIP := net.ParseIP(value.target.PrivateIPv4)
	if privateIP == nil || privateIP.To4() == nil || (!privateIP.IsPrivate() && !privateIP.IsLoopback()) {
		return config{}, errors.New("GUEST_PRIVATE_IPV4 must be a private or loopback IP address")
	}
	listenHost, _, _ := net.SplitHostPort(value.listenAddress)
	if !net.ParseIP(listenHost).Equal(privateIP) {
		return config{}, errors.New("GUEST_LISTEN_ADDR must bind GUEST_PRIVATE_IPV4")
	}
	return value, nil
}

func validatePrivateListenAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("GUEST_LISTEN_ADDR must include a private IP and port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) || ip.IsUnspecified() {
		return errors.New("GUEST_LISTEN_ADDR must bind a private or loopback IP address")
	}
	return nil
}

type currentBootSource struct {
	identity   guest.BootIdentity
	bootIDFile string
}

type linuxMountInspector struct{}

func (linuxMountInspector) InspectPersistentMount(_ context.Context, mountPoint string) (guest.PersistentMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return guest.PersistentMount{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if len(fields) < 6 || separator < 0 || separator+2 >= len(fields) || decodeMountInfoPath(fields[4]) != mountPoint {
			continue
		}
		volumeID, err := blockVolumeID(fields[separator+2])
		if err != nil {
			return guest.PersistentMount{}, fmt.Errorf("inspect mounted block volume identity: %w", err)
		}
		return guest.PersistentMount{
			Mounted: true, MountPoint: mountPoint, DeviceID: fields[2], VolumeID: volumeID,
			Writable: mountOption(fields[5], "rw"),
		}, nil
	}
	if err := scanner.Err(); err != nil {
		return guest.PersistentMount{}, err
	}
	return guest.PersistentMount{MountPoint: mountPoint}, nil
}

func decodeMountInfoPath(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func mountOption(options, expected string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == expected {
			return true
		}
	}
	return false
}

func blockVolumeID(source string) (string, error) {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	device := filepath.Base(resolved)
	candidates := []string{device}
	if index := strings.LastIndexByte(device, 'p'); index > 0 {
		if _, err := strconv.Atoi(device[index+1:]); err == nil {
			candidates = append(candidates, device[:index])
		}
	}
	for len(device) > 0 && device[len(device)-1] >= '0' && device[len(device)-1] <= '9' {
		device = device[:len(device)-1]
	}
	if device != "" {
		candidates = append(candidates, device)
	}
	for _, candidate := range candidates {
		serial, err := readOneLine(filepath.Join("/sys/class/block", candidate, "device", "serial"))
		if err != nil {
			continue
		}
		if strings.HasPrefix(serial, "vol") && !strings.HasPrefix(serial, "vol-") {
			serial = "vol-" + strings.TrimPrefix(serial, "vol")
		}
		return serial, nil
	}
	return "", errors.New("mounted block device has no readable serial")
}

func (source currentBootSource) CurrentBoot(context.Context) (guest.BootIdentity, error) {
	bootID, err := readOneLine(source.bootIDFile)
	if err != nil {
		return guest.BootIdentity{}, err
	}
	identity := source.identity
	identity.BootID = bootID
	return identity, nil
}

type ed25519HostIdentityGenerator struct{}

func (ed25519HostIdentityGenerator) GenerateEd25519HostIdentity(context.Context) (guest.GeneratedSSHHostIdentity, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return guest.GeneratedSSHHostIdentity{}, err
	}
	block, err := ssh.MarshalPrivateKey(private, "devm guest host identity")
	if err != nil {
		return guest.GeneratedSSHHostIdentity{}, err
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return guest.GeneratedSSHHostIdentity{}, err
	}
	return guest.GeneratedSSHHostIdentity{PrivateKey: pem.EncodeToMemory(block), PublicKey: ssh.MarshalAuthorizedKey(sshPublic)}, nil
}

type sshdReloader interface {
	Reload(context.Context) error
}

type sshHostKeyProber interface {
	Probe(context.Context, string, string) error
}

type sshdIdentityActivator struct {
	reloader sshdReloader
	prober   sshHostKeyProber
}

func (activator sshdIdentityActivator) ActivateAndVerify(ctx context.Context, target guestcontrol.Target, fingerprint string) error {
	if activator.reloader == nil || activator.prober == nil {
		return errors.New("sshd identity activator is not configured")
	}
	if err := activator.reloader.Reload(ctx); err != nil {
		return fmt.Errorf("reload sshd: %w", err)
	}
	if err := activator.prober.Probe(ctx, target.PrivateIPv4, fingerprint); err != nil {
		return fmt.Errorf("verify sshd host identity: %w", err)
	}
	return nil
}

type systemdSSHReloader struct{}

func (systemdSSHReloader) Reload(ctx context.Context) error {
	command := exec.CommandContext(ctx, "systemctl", "reload", "ssh.service")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

type tcpSSHHostKeyProber struct{ port int }

func (prober tcpSSHHostKeyProber) Probe(ctx context.Context, address, expectedFingerprint string) error {
	verified := false
	configuration := &ssh.ClientConfig{
		User: "dev", HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			actual := ssh.FingerprintSHA256(key)
			if actual != expectedFingerprint {
				return fmt.Errorf("served host key fingerprint %q does not match restored identity %q", actual, expectedFingerprint)
			}
			verified = true
			return nil
		},
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(prober.port)))
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	sshConnection, channels, requests, handshakeErr := ssh.NewClientConn(connection, net.JoinHostPort(address, strconv.Itoa(prober.port)), configuration)
	if sshConnection != nil {
		sshConnection.Close()
	}
	if channels != nil || requests != nil {
		// No client is constructed: the probe only verifies the key exchange.
	}
	if verified {
		return nil
	}
	if handshakeErr == nil {
		return errors.New("SSH handshake completed without a verified host key")
	}
	return handshakeErr
}

type jsonSSHKeySource struct{ path string }

func optionalSSHKeySource(path string) guestcontrol.SSHKeySource {
	if path == "" {
		return nil
	}
	return jsonSSHKeySource{path: path}
}

func (source jsonSSHKeySource) SSHKeys(_ context.Context, target guestcontrol.Target) ([]guest.EnvironmentSSHKey, error) {
	var input targetedInput[[]guest.EnvironmentSSHKey]
	if err := decodePrivateJSONFile(source.path, &input, os.Geteuid()); err != nil {
		return nil, err
	}
	if err := validateInputTarget(input.Target, target); err != nil {
		return nil, permanentInputError{err}
	}
	for _, key := range input.Value {
		if key.OwnerID != target.OwnerUserID {
			return nil, permanentInputError{errors.New("SSH key desired state contains a foreign owner")}
		}
	}
	return input.Value, nil
}

type jsonManagedConfigurationSource struct{ path string }

func optionalManagedConfigurationSource(path string) guestcontrol.ManagedConfigurationSource {
	if path == "" {
		return nil
	}
	return jsonManagedConfigurationSource{path: path}
}

func (source jsonManagedConfigurationSource) ManagedConfiguration(_ context.Context, target guestcontrol.Target) (guest.ProfileMaterializationBatch, error) {
	var input targetedInput[guest.ProfileMaterializationBatch]
	if err := decodePrivateJSONFile(source.path, &input, os.Geteuid()); err != nil {
		return guest.ProfileMaterializationBatch{}, err
	}
	if err := validateInputTarget(input.Target, target); err != nil {
		return guest.ProfileMaterializationBatch{}, permanentInputError{err}
	}
	return input.Value, nil
}

type targetedInput[T any] struct {
	Target guestcontrol.Target `json:"target"`
	Value  T                   `json:"value"`
}

type JSONActivitySampleSource struct {
	Path       string
	Target     guestcontrol.Target
	TrustedUID int
	MaxAge     time.Duration
	Now        func() time.Time

	mu           sync.Mutex
	lastSequence int64
}

func (source *JSONActivitySampleSource) Sample(context.Context) (guest.ExternalSample, error) {
	var input targetedInput[guest.ExternalSample]
	if err := decodePrivateJSONFile(source.Path, &input, source.TrustedUID); err != nil {
		return guest.ExternalSample{}, err
	}
	if err := validateInputTarget(input.Target, source.Target); err != nil {
		return guest.ExternalSample{}, permanentInputError{err}
	}
	now := time.Now().UTC()
	if source.Now != nil {
		now = source.Now().UTC()
	}
	maxAge := source.MaxAge
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	if input.Value.ObservedAt.After(now) || now.Sub(input.Value.ObservedAt) > maxAge {
		return guest.ExternalSample{}, errors.New("Activity Snapshot sample is stale or from the future")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if input.Value.GuestSequence < source.lastSequence {
		return guest.ExternalSample{}, errors.New("Activity Snapshot sequence regressed")
	}
	source.lastSequence = input.Value.GuestSequence
	return input.Value, nil
}

func decodePrivateJSONFile(path string, destination any, trustedUID int) error {
	file, before, err := openPrivateFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0o777 != 0o600 || before.Uid != 0 && before.Uid != uint32(trustedUID) {
		return permanentInputError{errors.New("guest input must be a trusted-owner 0600 regular file")}
	}
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("guest input must contain one JSON document")
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return err
	}
	if before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return errors.New("guest input changed while it was being read; publish files with atomic rename")
	}
	return nil
}

type permanentInputError struct{ error }

func (permanentInputError) Transient() bool { return false }

func openPrivateFile(name string) (*os.File, unix.Stat_t, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, unix.Stat_t{}, errors.New("guest input path must be absolute and clean")
	}
	parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	for index, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, unix.Stat_t{}, openErr
		}
		fd = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, unix.Stat_t{}, err
	}
	return os.NewFile(uintptr(fd), name), stat, nil
}

func validateInputTarget(actual, expected guestcontrol.Target) error {
	if actual != expected {
		return errors.New("guest input Target does not match the current boot")
	}
	return nil
}

func readOneLine(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(content))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("value must be one non-empty line")
	}
	return value, nil
}

func readAgentRequirements(path string) ([]guestcontrol.AgentRequirement, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("agent version manifest must be a non-writable regular file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	requirements := make([]guestcontrol.AgentRequirement, 0, len(lines))
	for index, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("agent version manifest line %d must contain name, executable, and version", index+1)
		}
		requirements = append(requirements, guestcontrol.AgentRequirement{
			Name: fields[0], Executable: fields[1], ExpectedVersion: fields[2],
		})
	}
	if len(requirements) == 0 {
		return nil, errors.New("agent version manifest is empty")
	}
	return requirements, nil
}

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(name string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(name, uid, gid)
	})
}

func positiveInt(name string) (int, error) {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func intOrDefault(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
