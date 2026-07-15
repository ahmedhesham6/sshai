package provideraws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const (
	tagEnvironment     = "sshai.io/environment"
	tagEnvironmentID   = "sshai.io/environment-id"
	tagManagedBy       = "sshai.io/managed-by"
	tagOperationID     = "sshai.io/operation-id"
	tagRegion          = "sshai.io/region"
	tagResource        = "sshai.io/resource"
	tagRuntimeID       = "sshai.io/runtime-id"
	tagRuntimeSequence = "sshai.io/runtime-sequence"
	tagRuntimePreset   = "sshai.io/runtime-preset"
	tagImageVersion    = "sshai.io/image-version"
	tagDataVolumeID    = "sshai.io/data-volume-id"

	managedByValue     = "sshai"
	dataVolumeResource = "data-volume"
)

type Config struct {
	Region      string
	Environment string
	SizeGiB     int32
	EndpointURL string
	Runtime     RuntimeConfig
}

type RuntimeConfig struct {
	AMI             string
	Presets         map[string]string
	SubnetID        string
	SecurityGroupID string
	SystemVolumeGiB int32
}

type ec2API interface {
	AttachVolume(context.Context, *ec2.AttachVolumeInput, ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error)
	CreateVolume(context.Context, *ec2.CreateVolumeInput, ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DetachVolume(context.Context, *ec2.DetachVolumeInput, ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

type Provider struct {
	client      ec2API
	region      string
	environment string
	sizeGiB     int32
	runtime     RuntimeConfig
}

var _ provider.DataVolumeProvider = (*Provider)(nil)

func New(ctx context.Context, adapterConfig Config) (*Provider, error) {
	if strings.TrimSpace(adapterConfig.Region) == "" || strings.TrimSpace(adapterConfig.Environment) == "" || adapterConfig.SizeGiB < 1 {
		return nil, errors.New("AWS provider requires region, environment, and positive Data Volume size")
	}
	loadOptions := []func(*config.LoadOptions) error{config.WithRegion(adapterConfig.Region)}
	sdkConfig, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := ec2.NewFromConfig(sdkConfig, func(options *ec2.Options) {
		if adapterConfig.EndpointURL != "" {
			options.BaseEndpoint = aws.String(adapterConfig.EndpointURL)
		}
	})
	return newProvider(client, adapterConfig)
}

func newProvider(client ec2API, adapterConfig Config) (*Provider, error) {
	if client == nil {
		return nil, errors.New("AWS provider requires an EC2 client")
	}
	adapterConfig.Runtime.Presets = maps.Clone(adapterConfig.Runtime.Presets)
	return &Provider{
		client: client, region: adapterConfig.Region, environment: adapterConfig.Environment,
		sizeGiB: adapterConfig.SizeGiB, runtime: adapterConfig.Runtime,
	}, nil
}

func (adapter *Provider) EnsureDataVolume(ctx context.Context, request provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	if err := adapter.validateRequest(request); err != nil {
		return provider.DataVolume{}, err
	}
	volumes, err := adapter.findDataVolumes(ctx, request.EnvironmentID)
	if err != nil {
		return provider.DataVolume{}, containError("describe Data Volumes", err)
	}
	switch len(volumes) {
	case 0:
		return adapter.createDataVolume(ctx, request)
	case 1:
		return adapter.ownedDataVolume(request, volumes[0])
	default:
		return provider.DataVolume{}, provider.NewError(
			provider.ErrorCodeResourceDiverged,
			fmt.Sprintf("Environment %q has multiple Data Volumes", request.EnvironmentID), nil,
		)
	}
}

func (adapter *Provider) validateRequest(request provider.EnsureDataVolumeRequest) error {
	if strings.TrimSpace(request.EnvironmentID) == "" || strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.Region) == "" || strings.TrimSpace(request.AvailabilityZone) == "" {
		return provider.NewError(provider.ErrorCodeInvalidRequest, "Environment, Operation, region, and availability zone are required", nil)
	}
	if request.Region != adapter.region {
		return provider.NewError(
			provider.ErrorCodePlacementConflict,
			fmt.Sprintf("requested region %q does not match adapter region %q", request.Region, adapter.region), nil,
		)
	}
	return nil
}

func (adapter *Provider) findDataVolumes(ctx context.Context, environmentID string) ([]types.Volume, error) {
	input := &ec2.DescribeVolumesInput{Filters: []types.Filter{{
		Name: aws.String("tag:" + tagEnvironmentID), Values: []string{environmentID},
	}}}
	var volumes []types.Volume
	for {
		output, err := adapter.client.DescribeVolumes(ctx, input)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, output.Volumes...)
		if output.NextToken == nil || *output.NextToken == "" {
			return volumes, nil
		}
		input.NextToken = output.NextToken
	}
}

func (adapter *Provider) createDataVolume(ctx context.Context, request provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	output, err := adapter.client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(request.AvailabilityZone),
		ClientToken:      aws.String(providerToken(dataVolumeResource, request.EnvironmentID)),
		Encrypted:        aws.Bool(true),
		Size:             aws.Int32(adapter.sizeGiB),
		VolumeType:       types.VolumeTypeGp3,
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeVolume,
			Tags:         adapter.ownershipTags(request.EnvironmentID, request.OperationID, request.Region, dataVolumeResource),
		}},
	})
	if err != nil {
		return provider.DataVolume{}, containError("create Data Volume", err)
	}
	return adapter.ownedDataVolume(request, types.Volume{
		AvailabilityZone: output.AvailabilityZone,
		Encrypted:        output.Encrypted,
		Size:             output.Size,
		Tags:             output.Tags,
		VolumeId:         output.VolumeId,
		VolumeType:       output.VolumeType,
	})
}

func (adapter *Provider) ownedDataVolume(request provider.EnsureDataVolumeRequest, volume types.Volume) (provider.DataVolume, error) {
	if aws.ToString(volume.AvailabilityZone) != request.AvailabilityZone {
		return provider.DataVolume{}, provider.NewError(
			provider.ErrorCodePlacementConflict,
			fmt.Sprintf("Environment %q Data Volume is in availability zone %q", request.EnvironmentID, aws.ToString(volume.AvailabilityZone)), nil,
		)
	}
	tags := tagValues(volume.Tags)
	wantTags := map[string]string{
		tagEnvironment: adapter.environment, tagEnvironmentID: request.EnvironmentID,
		tagManagedBy: managedByValue, tagRegion: request.Region, tagResource: dataVolumeResource,
	}
	for key, value := range wantTags {
		if tags[key] != value {
			return provider.DataVolume{}, provider.NewError(
				provider.ErrorCodeResourceDiverged,
				fmt.Sprintf("Environment %q Data Volume ownership is not valid", request.EnvironmentID), nil,
			)
		}
	}
	if tags[tagOperationID] == "" || !aws.ToBool(volume.Encrypted) || volume.VolumeType != types.VolumeTypeGp3 || aws.ToInt32(volume.Size) != adapter.sizeGiB || aws.ToString(volume.VolumeId) == "" {
		return provider.DataVolume{}, provider.NewError(
			provider.ErrorCodeResourceDiverged,
			fmt.Sprintf("Environment %q Data Volume configuration diverged", request.EnvironmentID), nil,
		)
	}
	return provider.DataVolume{
		Provider: "aws", ProviderID: aws.ToString(volume.VolumeId), EnvironmentID: request.EnvironmentID,
		Region: request.Region, AvailabilityZone: request.AvailabilityZone,
	}, nil
}

func (adapter *Provider) ownershipTags(environmentID, operationID, region, resource string) []types.Tag {
	values := [...]struct{ key, value string }{
		{tagEnvironment, adapter.environment},
		{tagEnvironmentID, environmentID},
		{tagManagedBy, managedByValue},
		{tagOperationID, operationID},
		{tagRegion, region},
		{tagResource, resource},
	}
	tags := make([]types.Tag, 0, len(values))
	for _, value := range values {
		tags = append(tags, types.Tag{Key: aws.String(value.key), Value: aws.String(value.value)})
	}
	return tags
}

func tagValues(tags []types.Tag) map[string]string {
	values := make(map[string]string, len(tags))
	for _, tag := range tags {
		values[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return values
}

func providerToken(resource, environmentID string) string {
	digest := sha256.Sum256([]byte(resource + "\x00" + environmentID))
	return "sshai-" + hex.EncodeToString(digest[:])[:56]
}

func containError(operation string, err error) error {
	code := provider.ErrorCodeUnavailable
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "UnauthorizedOperation", "AuthFailure", "AccessDenied":
			code = provider.ErrorCodeAuthorizationFailed
		case "RequestLimitExceeded", "Throttling", "ThrottlingException":
			code = provider.ErrorCodeRateLimited
		case "InsufficientInstanceCapacity", "InsufficientHostCapacity":
			code = provider.ErrorCodeCapacityUnavailable
		case "InvalidParameter", "InvalidParameterValue", "MissingParameter":
			code = provider.ErrorCodeInvalidRequest
		case "IdempotentParameterMismatch":
			code = provider.ErrorCodeResourceDiverged
		}
	}
	return provider.NewError(code, operation+" failed", err)
}
