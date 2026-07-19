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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
	"golang.org/x/crypto/ssh"
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
	if _, err := reporter.Advance(ctx, guest.ReadinessDataMounted, time.Now().UTC()); err != nil {
		return fmt.Errorf("record mounted State Components: %w", err)
	}

	var observer *guest.Observer
	if config.activitySampleFile != "" {
		observer, err = guest.NewObserver(config.target.RuntimeID, JSONActivitySampleSource{Path: config.activitySampleFile}, guest.Allowlists{
			CodexExecutables: splitList(config.codexExecutables), ClaudeExecutables: splitList(config.claudeExecutables),
			ProtectedExecutables: splitList(config.protectedExecutables), SelectedContainers: splitList(config.selectedContainers),
		})
		if err != nil {
			return fmt.Errorf("construct Activity Snapshot observer: %w", err)
		}
	}

	operations, err := guestcontrol.NewLocalOperations(guestcontrol.LocalOperationsConfig{
		Target: config.target, Readiness: reporter,
		WorkspaceRoot: config.workspaceRoot, HomeRoot: config.homeRoot, CacheRoot: config.cacheRoot,
		PlatformRoot: config.platformRoot, SSHDRoot: config.sshdRoot,
		HostIdentityGenerator: ed25519HostIdentityGenerator{},
		SSHKeys:               optionalSSHKeySource(config.authorizedKeysFile),
		ManagedConfiguration:  optionalManagedConfigurationSource(config.managedConfigurationFile),
		Activity:              observer,
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
		EnvironmentID: config.target.EnvironmentID, ClientIdentity: config.clientIdentity,
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
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute,
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
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down guest control API: %w", err)
		}
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

	authorizedKeysFile       string
	managedConfigurationFile string
	activitySampleFile       string
	codexExecutables         string
	claudeExecutables        string
	protectedExecutables     string
	selectedContainers       string
}

func loadConfig() (config, error) {
	runtimeSequence, err := strconv.ParseInt(os.Getenv("GUEST_RUNTIME_SEQUENCE"), 10, 64)
	if err != nil || runtimeSequence < 1 {
		return config{}, errors.New("GUEST_RUNTIME_SEQUENCE must be a positive integer")
	}
	dataRoot := valueOrDefault("GUEST_DATA_ROOT", "/var/lib/devm")
	value := config{
		listenAddress:   valueOrDefault("GUEST_LISTEN_ADDR", "127.0.0.1:9443"),
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
		sshdRoot:           valueOrDefault("GUEST_SSHD_ROOT", "/etc/ssh"),
		authorizedKeysFile: os.Getenv("GUEST_AUTHORIZED_KEYS_FILE"), managedConfigurationFile: os.Getenv("GUEST_MANAGED_CONFIGURATION_FILE"),
		activitySampleFile: os.Getenv("GUEST_ACTIVITY_SAMPLE_FILE"),
		codexExecutables:   os.Getenv("GUEST_CODEX_EXECUTABLES"), claudeExecutables: os.Getenv("GUEST_CLAUDE_EXECUTABLES"),
		protectedExecutables: os.Getenv("GUEST_PROTECTED_EXECUTABLES"), selectedContainers: os.Getenv("GUEST_SELECTED_CONTAINERS"),
	}
	if value.certificateFile == "" || value.privateKeyFile == "" || value.clientCAFile == "" || value.clientIdentity == "" ||
		value.target.OwnerUserID == "" || value.target.EnvironmentID == "" || value.target.RuntimeID == "" || value.target.ProviderID == "" || value.target.PrivateIPv4 == "" ||
		value.dataDeviceID == "" || value.dataVolumeID == "" {
		return config{}, errors.New("GUEST_TLS_CERT_FILE, GUEST_TLS_KEY_FILE, GUEST_TLS_CLIENT_CA_FILE, GUEST_TLS_CLIENT_URI, GUEST_OWNER_USER_ID, GUEST_ENVIRONMENT_ID, GUEST_RUNTIME_ID, GUEST_PROVIDER_ID, GUEST_PRIVATE_IPV4, GUEST_DATA_DEVICE_ID, and GUEST_DATA_VOLUME_ID are required")
	}
	if err := validatePrivateListenAddress(value.listenAddress); err != nil {
		return config{}, err
	}
	privateIP := net.ParseIP(value.target.PrivateIPv4)
	if privateIP == nil || (!privateIP.IsPrivate() && !privateIP.IsLoopback()) {
		return config{}, errors.New("GUEST_PRIVATE_IPV4 must be a private or loopback IP address")
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

type jsonSSHKeySource struct{ path string }

func optionalSSHKeySource(path string) guestcontrol.SSHKeySource {
	if path == "" {
		return nil
	}
	return jsonSSHKeySource{path: path}
}

func (source jsonSSHKeySource) SSHKeys(context.Context, guestcontrol.Target) ([]guest.EnvironmentSSHKey, error) {
	var keys []guest.EnvironmentSSHKey
	if err := decodePrivateJSONFile(source.path, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

type jsonManagedConfigurationSource struct{ path string }

func optionalManagedConfigurationSource(path string) guestcontrol.ManagedConfigurationSource {
	if path == "" {
		return nil
	}
	return jsonManagedConfigurationSource{path: path}
}

func (source jsonManagedConfigurationSource) ManagedConfiguration(context.Context, guestcontrol.Target) (guest.ProfileMaterializationBatch, error) {
	var batch guest.ProfileMaterializationBatch
	if err := decodePrivateJSONFile(source.path, &batch); err != nil {
		return guest.ProfileMaterializationBatch{}, err
	}
	return batch, nil
}

type JSONActivitySampleSource struct{ Path string }

func (source JSONActivitySampleSource) Sample(context.Context) (guest.ExternalSample, error) {
	var sample guest.ExternalSample
	if err := decodePrivateJSONFile(source.Path, &sample); err != nil {
		return guest.ExternalSample{}, err
	}
	return sample, nil
}

func decodePrivateJSONFile(path string, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return errors.New("guest input must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("guest input must contain one JSON document")
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
