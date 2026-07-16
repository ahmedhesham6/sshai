package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	controlplane "github.com/ahmedhesham6/sshai/apps/control-plane"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/application"
	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/provideraws"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/restatedev/sdk-go/ingress"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	database, err := sql.Open("pgx", config.databaseURL)
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, config.databaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()
	store := db.NewStore(pool)
	verifier, err := auth.NewVerifier(ctx, auth.VerifierConfig{
		JWKSURL: config.workOSJWKSURL, Issuer: config.workOSIssuer, ClientID: config.workOSClientID,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return err
	}
	defer verifier.Close(context.Background())
	workflowClient := workflows.NewClient(ingress.NewClient(config.restateIngressURL))
	dispatcher := application.NewEnvironmentCreateDispatcher(store, workflowClient)
	recovery := application.NewWorkflowRecovery(dispatcher, 5*time.Second, 100, func(err error) {
		slog.Error("workflow recovery failed", "error", err)
	})
	go func() {
		if err := recovery.Run(ctx); err != nil {
			slog.Error("workflow recovery stopped", "error", err)
		}
	}()
	ids := uuidGenerator{}
	uploadStore, err := provideraws.NewUploadStore(ctx, provideraws.UploadConfig{
		Region: config.defaultRegion, Bucket: config.uploadBucket, EndpointURL: config.s3EndpointURL,
	})
	if err != nil {
		return err
	}
	const maxSingleObjectBytes = int64(5_000_000_000)
	uploads := application.NewUploadIntentService(
		store, uploadStore, uploadStore, uploadStore, ids, time.Now, 10*time.Minute,
		map[domain.UploadKind]int64{
			domain.UploadProfileArtifact: maxSingleObjectBytes,
			domain.UploadGitBundle:       maxSingleObjectBytes,
			domain.UploadTrackedPatch:    maxSingleObjectBytes,
			domain.UploadUntrackedBundle: maxSingleObjectBytes,
			domain.UploadSeedManifest:    maxSingleObjectBytes,
		},
	)
	awsSDKConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(config.defaultRegion))
	if err != nil {
		return fmt.Errorf("load Capsule store AWS configuration: %w", err)
	}
	capsuleClient := s3.NewFromConfig(awsSDKConfig, func(options *s3.Options) {
		if config.s3EndpointURL != "" {
			options.BaseEndpoint = aws.String(config.s3EndpointURL)
			options.UsePathStyle = true
		}
	})
	capsulePresigner := s3.NewPresignClient(capsuleClient)
	createEnvironment := application.NewCreateEnvironmentService(
		store, dispatcher, ids, time.Now,
		map[string]string{config.defaultRegion: config.defaultAvailabilityZone},
	)
	registerProjectSeed := application.NewRegisterProjectSeedService(store, uploads, ids, time.Now)
	profiles := application.NewProfileService(store, uploads, ids, time.Now)
	sshKeys := application.NewSSHKeyService(store, ids, time.Now)
	handler := controlplane.NewHandler(controlplane.Config{
		CreateEnvironment: createEnvironment, RegisterProjectSeed: registerProjectSeed, Profiles: profiles, Uploads: uploads, SSHKeys: sshKeys,
		Verifier: verifier, Users: store, CapsulePresigner: capsulePresigner, CapsuleOwnership: controlplane.NewS3CapsuleOwnership(capsuleClient, config.capsuleBucket), CapsuleBucket: config.capsuleBucket, CapsuleAccessTTL: 15 * time.Minute,
		UserIDs: ids, RequestIDs: ids, DefaultRegion: config.defaultRegion, Now: time.Now,
	})
	server := &http.Server{Addr: config.listenAddress, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve control plane: %w", err)
	}
	return nil
}

type config struct {
	databaseURL             string
	workOSClientID          string
	workOSIssuer            string
	workOSJWKSURL           string
	restateIngressURL       string
	defaultRegion           string
	defaultAvailabilityZone string
	listenAddress           string
	uploadBucket            string
	capsuleBucket           string
	s3EndpointURL           string
}

func loadConfig() (config, error) {
	clientID := os.Getenv("WORKOS_CLIENT_ID")
	config := config{
		databaseURL: os.Getenv("DATABASE_URL"), workOSClientID: clientID,
		workOSIssuer:            valueOrDefault("WORKOS_ISSUER", "https://api.workos.com/"),
		workOSJWKSURL:           valueOrDefault("WORKOS_JWKS_URL", "https://api.workos.com/sso/jwks/"+clientID),
		restateIngressURL:       valueOrDefault("RESTATE_INGRESS_URL", "http://localhost:8080"),
		defaultRegion:           valueOrDefault("DEFAULT_REGION", "us-east-1"),
		defaultAvailabilityZone: valueOrDefault("DEFAULT_AVAILABILITY_ZONE", "us-east-1a"),
		listenAddress:           valueOrDefault("LISTEN_ADDR", ":8081"),
		uploadBucket:            os.Getenv("UPLOAD_BUCKET"), capsuleBucket: os.Getenv("CAPSULE_BUCKET"),
		s3EndpointURL: os.Getenv("AWS_ENDPOINT_URL_S3"),
	}
	if config.databaseURL == "" || config.workOSClientID == "" || config.uploadBucket == "" || config.capsuleBucket == "" {
		return config, errors.New("DATABASE_URL, WORKOS_CLIENT_ID, UPLOAD_BUCKET, and CAPSULE_BUCKET are required")
	}
	return config, nil
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type uuidGenerator struct{}

func (uuidGenerator) NewID() string { return uuid.NewString() }
