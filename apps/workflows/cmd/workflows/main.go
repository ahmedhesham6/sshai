// Command workflows serves the Restate workflow services over HTTP so a
// Restate instance can invoke them. Registering this deployment with that
// instance (`restate deployments register <url>`, or the Restate Cloud UI)
// is an operational step outside this binary.
//
// BillingDelivery, EnvironmentCreate, and ProfileResolve are served today;
// see serviceDependencies for the production seam Auto-stop is still
// waiting on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/provideraws"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
	"github.com/restatedev/sdk-go/server"
)

// capsuleGrantTTL bounds how long a minted Capsule read/write grant is
// valid, mirroring apps/control-plane's default capsule access TTL
// (CapsuleAccessTTL in controlplane.Config).
const capsuleGrantTTL = 15 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("workflows service stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, config.databaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()
	store := db.NewStore(pool)
	workflowClient := workflows.NewClient(ingress.NewClient(config.restateIngressURL))
	polarClient, err := billing.NewPolarEventClient(
		config.polarEventsEndpoint, config.polarAccessToken, &http.Client{Timeout: 10 * time.Second},
	)
	if err != nil {
		return err
	}

	awsSDKConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(config.defaultRegion))
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	capsuleClient := s3.NewFromConfig(awsSDKConfig, func(options *s3.Options) {
		if config.s3EndpointURL != "" {
			options.BaseEndpoint = aws.String(config.s3EndpointURL)
			options.UsePathStyle = true
		}
	})
	grantProvider, err := oci.NewS3GrantProvider(s3.NewPresignClient(capsuleClient), config.capsuleBucket, capsuleGrantTTL)
	if err != nil {
		return fmt.Errorf("construct Capsule grant provider: %w", err)
	}
	capsuleResolver := newCapsuleResolverAdapter(oci.NewResolver(grantProvider))

	pinnedResolver := newPinnedProfileVersionResolver(store, store, capsuleResolver, idGenerator{})
	creationActions, err := workflows.NewEnvironmentCreationActions(store, pinnedResolver)
	if err != nil {
		return fmt.Errorf("construct Environment creation actions: %w", err)
	}
	runtimeProvider, err := provideraws.New(ctx, provideraws.Config{
		Region: config.defaultRegion, Environment: config.runtimeEnvironmentName,
		SizeGiB: config.dataVolumeSizeGiB, EndpointURL: config.ec2EndpointURL,
		Runtime: provideraws.RuntimeConfig{
			AMI: config.runtimeAMI, Presets: config.runtimePresets,
			SubnetID: config.runtimeSubnetID, SecurityGroupID: config.runtimeSecurityGroupID,
			SystemVolumeGiB: config.runtimeSystemVolumeGiB,
		},
	})
	if err != nil {
		return fmt.Errorf("construct Runtime provider: %w", err)
	}

	profileResolveActions := workflows.NewProfileResolveActions(&profileResolveStateStore{Store: store})
	runtimeActions := &runtimeWorkflowActions{store: store, now: time.Now}
	snapshots := newAutoStopSnapshotSource(store)
	dataVolumes := runtimeDataVolumeVerifier{store: store}
	guest, err := newRuntimeGuestTransport(config.guestControl, guestTransportDependencies{
		projectSeeds: storedProjectSeedSource{
			metadata: store,
			objects:  s3ProjectSeedObjectSource{client: capsuleClient, bucket: config.capsuleBucket},
		},
		capsuleGrants: capsuleMaterializationGrantSource{provider: grantProvider},
	})
	if err != nil {
		return fmt.Errorf("construct guest control transport: %w", err)
	}
	runtimeDispatcher := application.NewRuntimeOperationDispatcher(store, workflowClient)
	runtimeCommands := application.NewRuntimeCommandService(store, runtimeDispatcher, store, idGenerator{}, time.Now)
	autoStop := workflows.AutoStopDefinition(snapshots, runtimeStopDispatcher{store: store, commands: runtimeCommands})
	runtimeStart := workflows.RuntimeStartDefinition(workflows.RuntimeStartDependencies{
		Provider: runtimeProvider, Attachments: runtimeProvider, Actions: runtimeActions, DataVolumes: dataVolumes, Credits: store,
		Images: promotedImageSource{image: config.imageVersion}, Usage: store, Guest: guest, SSHKeys: guest,
		Managed: guest, ReplacementActions: runtimeActions, HostIdentity: guest,
		AutoStop: workflowClient, IDs: idGenerator{}, Now: time.Now,
	})
	runtimeStop := workflows.RuntimeStopDefinition(workflows.RuntimeStopDependencies{
		Provider: runtimeProvider, Actions: runtimeActions, DataVolumes: dataVolumes, Snapshots: snapshots,
		Guest: guest, Usage: store, AutoStop: workflowClient, Now: time.Now,
	})
	runtimeReplace := workflows.RuntimeReplaceDefinition(workflows.RuntimeReplaceDependencies{
		Provider: runtimeProvider, Attachments: runtimeProvider, Actions: runtimeActions, DataVolumes: dataVolumes,
		Images: promotedImageSource{image: config.imageVersion}, Usage: store, Guest: guest,
		HostIdentity: guest, SSHKeys: guest, Managed: guest, AutoStop: workflowClient,
		IDs: idGenerator{}, Now: time.Now,
	})

	restateServer := server.NewRestate()
	for _, service := range buildServices(serviceDependencies{
		polarDeliverer: polarClient, polarStore: store, now: time.Now,
		environmentCreate: workflows.EnvironmentCreateDefinitionWithDependencies(workflows.EnvironmentCreateDependencies{
			Provider: runtimeProvider, Actions: creationActions, Capsules: creationActions,
			SSHIdentity: guest, GuestReadiness: guest, SSHKeys: guest, ProjectSeed: guest, Materializer: guest,
			Credentials: workflows.NoProjectCredentialBinder{}, Toolchain: guest,
			IDs: idGenerator{}, Now: time.Now, ImageVersion: config.imageVersion,
		}),
		profileResolve: workflows.ProfileResolveDefinition(profileResolveActions, capsuleResolver, idGenerator{}, time.Now),
		autoStop:       autoStop,
		runtimeStart:   runtimeStart,
		runtimeStop:    runtimeStop,
		runtimeReplace: runtimeReplace,
	}) {
		restateServer.Bind(service)
	}
	if err := restateServer.Start(ctx, config.listenAddress); err != nil {
		return fmt.Errorf("serve workflows: %w", err)
	}
	return nil
}

// serviceDependencies carries the production implementations behind each
// workflow service. Every production definition is built in run; the
// nullable fields keep buildServices easy to exercise in focused tests.
type serviceDependencies struct {
	polarDeliverer workflows.PolarEventDeliverer
	polarStore     workflows.PolarDeliveryStore
	now            func() time.Time

	environmentCreate restate.ServiceDefinition
	profileResolve    restate.ServiceDefinition
	autoStop          restate.ServiceDefinition
	runtimeStart      restate.ServiceDefinition
	runtimeStop       restate.ServiceDefinition
	runtimeReplace    restate.ServiceDefinition
}

func buildServices(dependencies serviceDependencies) []restate.ServiceDefinition {
	services := []restate.ServiceDefinition{
		workflows.BillingDeliveryDefinition(dependencies.polarDeliverer, dependencies.polarStore, dependencies.now),
	}
	for _, pending := range []restate.ServiceDefinition{
		dependencies.environmentCreate, dependencies.profileResolve, dependencies.autoStop,
		dependencies.runtimeStart, dependencies.runtimeStop, dependencies.runtimeReplace,
	} {
		if pending != nil {
			services = append(services, pending)
		}
	}
	return services
}

type config struct {
	databaseURL         string
	polarEventsEndpoint string
	polarAccessToken    string
	listenAddress       string
	restateIngressURL   string

	defaultRegion string
	s3EndpointURL string
	capsuleBucket string

	ec2EndpointURL         string
	runtimeEnvironmentName string
	dataVolumeSizeGiB      int32
	runtimeAMI             string
	runtimeSubnetID        string
	runtimeSecurityGroupID string
	runtimeSystemVolumeGiB int32
	runtimePresets         map[string]string
	imageVersion           string
	guestControl           guestControlConfig
}

func loadConfig() (config, error) {
	presets, err := parseRuntimePresets(os.Getenv("RUNTIME_PRESETS"))
	if err != nil {
		return config{}, err
	}
	dataVolumeSizeGiB, err := int32OrDefault("DATA_VOLUME_SIZE_GIB", 100)
	if err != nil {
		return config{}, err
	}
	runtimeSystemVolumeGiB, err := int32OrDefault("RUNTIME_SYSTEM_VOLUME_GIB", 30)
	if err != nil {
		return config{}, err
	}
	guestControlPort, err := int32OrDefault("GUEST_CONTROL_PORT", 9443)
	if err != nil || guestControlPort < 1 || guestControlPort > 65535 {
		return config{}, errors.New("GUEST_CONTROL_PORT must be between 1 and 65535")
	}
	config := config{
		databaseURL:         os.Getenv("DATABASE_URL"),
		polarEventsEndpoint: os.Getenv("POLAR_EVENTS_ENDPOINT"),
		polarAccessToken:    os.Getenv("POLAR_ACCESS_TOKEN"),
		listenAddress:       valueOrDefault("LISTEN_ADDR", ":9080"),
		restateIngressURL:   valueOrDefault("RESTATE_INGRESS_URL", "http://localhost:8080"),

		defaultRegion: valueOrDefault("DEFAULT_REGION", "eu-central-1"),
		s3EndpointURL: os.Getenv("AWS_ENDPOINT_URL_S3"),
		capsuleBucket: os.Getenv("CAPSULE_BUCKET"),

		ec2EndpointURL:         os.Getenv("AWS_ENDPOINT_URL_EC2"),
		runtimeEnvironmentName: os.Getenv("RUNTIME_ENVIRONMENT_NAME"),
		dataVolumeSizeGiB:      dataVolumeSizeGiB,
		runtimeAMI:             os.Getenv("RUNTIME_AMI"),
		runtimeSubnetID:        os.Getenv("RUNTIME_SUBNET_ID"),
		runtimeSecurityGroupID: os.Getenv("RUNTIME_SECURITY_GROUP_ID"),
		runtimeSystemVolumeGiB: runtimeSystemVolumeGiB,
		runtimePresets:         presets,
		imageVersion:           os.Getenv("IMAGE_VERSION"),
		guestControl: guestControlConfig{
			port: int(guestControlPort), serverName: os.Getenv("GUEST_CONTROL_TLS_SERVER_NAME"), certificateDirectory: os.Getenv("GUEST_CONTROL_TLS_CERT_DIR"),
			privateKeyDirectory: os.Getenv("GUEST_CONTROL_TLS_KEY_DIR"), caFile: os.Getenv("GUEST_CONTROL_TLS_CA_FILE"),
		},
	}
	if config.databaseURL == "" || config.polarEventsEndpoint == "" || config.polarAccessToken == "" ||
		config.capsuleBucket == "" || config.runtimeEnvironmentName == "" || config.runtimeAMI == "" ||
		config.runtimeSubnetID == "" || config.runtimeSecurityGroupID == "" || config.imageVersion == "" || len(config.runtimePresets) == 0 {
		return config, errors.New(
			"DATABASE_URL, POLAR_EVENTS_ENDPOINT, POLAR_ACCESS_TOKEN, CAPSULE_BUCKET, RUNTIME_ENVIRONMENT_NAME, " +
				"RUNTIME_AMI, RUNTIME_SUBNET_ID, RUNTIME_SECURITY_GROUP_ID, RUNTIME_PRESETS, and IMAGE_VERSION are required",
		)
	}
	return config, nil
}

// parseRuntimePresets decodes a "name=instanceType,name2=instanceType2"
// value into a Runtime preset map, as consumed by
// provideraws.RuntimeConfig.Presets.
func parseRuntimePresets(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	presets := make(map[string]string)
	for _, pair := range strings.Split(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, instanceType, found := strings.Cut(pair, "=")
		name, instanceType = strings.TrimSpace(name), strings.TrimSpace(instanceType)
		if !found || name == "" || instanceType == "" {
			return nil, fmt.Errorf("RUNTIME_PRESETS entry %q must have the form name=instanceType", pair)
		}
		presets[name] = instanceType
	}
	return presets, nil
}

func int32OrDefault(name string, fallback int32) (int32, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return int32(parsed), nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
