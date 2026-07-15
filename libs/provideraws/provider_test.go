package provideraws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const miniStackImage = "ministackorg/ministack:1.3.14@sha256:1cb5a22e2028c43e7cc4bbbcce20cc8c8af7fbec449354c3a66db4bcf61e3615"

func TestEnsureDataVolumeCreatesEncryptedOwnedGP3VolumeOnce(t *testing.T) {
	adapter, ec2Client := startAdapter(t)
	request := provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}

	first, err := adapter.EnsureDataVolume(t.Context(), request)
	if err != nil {
		t.Fatalf("EnsureDataVolume(): %v", err)
	}
	request.OperationID = "operation-2"
	second, err := adapter.EnsureDataVolume(t.Context(), request)
	if err != nil {
		t.Fatalf("reconcile EnsureDataVolume(): %v", err)
	}
	if second != first {
		t.Fatalf("reconciled Data Volume = %#v, want %#v", second, first)
	}

	volumes := describeVolumes(t, ec2Client)
	if len(volumes) != 1 {
		t.Fatalf("EBS volumes = %d, want 1", len(volumes))
	}
	volume := volumes[0]
	if first.Provider != "aws" || first.ProviderID != aws.ToString(volume.VolumeId) || first.EnvironmentID != request.EnvironmentID || first.Region != request.Region || first.AvailabilityZone != request.AvailabilityZone {
		t.Fatalf("Data Volume = %#v; EBS volume = %#v", first, volume)
	}
	if !aws.ToBool(volume.Encrypted) || volume.VolumeType != types.VolumeTypeGp3 || aws.ToInt32(volume.Size) != 100 {
		t.Fatalf("EBS shape = encrypted:%t type:%s size:%d", aws.ToBool(volume.Encrypted), volume.VolumeType, aws.ToInt32(volume.Size))
	}
	wantTags := []string{
		"sshai.io/environment=development",
		"sshai.io/environment-id=environment-1",
		"sshai.io/managed-by=sshai",
		"sshai.io/operation-id=operation-1",
		"sshai.io/region=us-east-1",
		"sshai.io/resource=data-volume",
	}
	slices.Sort(wantTags)
	gotTags := make([]string, 0, len(volume.Tags))
	for _, tag := range volume.Tags {
		gotTags = append(gotTags, aws.ToString(tag.Key)+"="+aws.ToString(tag.Value))
	}
	slices.Sort(gotTags)
	if !slices.Equal(gotTags, wantTags) {
		t.Fatalf("EBS tags = %v, want %v", gotTags, wantTags)
	}
}

func TestEnsureDataVolumeRejectsPlacementConflictWithoutCreatingState(t *testing.T) {
	adapter, ec2Client := startAdapter(t)
	request := provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}
	if _, err := adapter.EnsureDataVolume(t.Context(), request); err != nil {
		t.Fatalf("EnsureDataVolume(): %v", err)
	}
	request.AvailabilityZone = "us-east-1b"
	_, err := adapter.EnsureDataVolume(t.Context(), request)
	assertProviderError(t, err, provider.ErrorCodePlacementConflict)
	if got := len(describeVolumes(t, ec2Client)); got != 1 {
		t.Fatalf("EBS volumes after placement conflict = %d, want 1", got)
	}
}

func TestEnsureDataVolumeContainsProviderFailures(t *testing.T) {
	setMiniStackCredentials(t)
	adapter, err := New(t.Context(), Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		EndpointURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	_, err = adapter.EnsureDataVolume(t.Context(), provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	})
	assertProviderError(t, err, provider.ErrorCodeUnavailable)
}

func startAdapter(t *testing.T) (*Provider, *ec2.Client) {
	t.Helper()
	setMiniStackCredentials(t)
	container, err := testcontainers.Run(t.Context(), miniStackImage,
		testcontainers.WithExposedPorts("4566/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/_ministack/ready").WithPort("4566/tcp")),
	)
	if err != nil {
		t.Fatalf("start MiniStack: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate MiniStack: %v", err)
		}
	})
	endpoint, err := container.Endpoint(t.Context(), "http")
	if err != nil {
		t.Fatalf("MiniStack endpoint: %v", err)
	}
	adapter, err := New(t.Context(), Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100, EndpointURL: endpoint,
		Runtime: RuntimeConfig{
			AMI: "ami-0123456789abcdef0", Presets: map[string]string{"standard": "m7i.xlarge"},
			SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
		},
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	sdkConfig, err := config.LoadDefaultConfig(t.Context(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("load AWS SDK config: %v", err)
	}
	client := ec2.NewFromConfig(sdkConfig, func(options *ec2.Options) { options.BaseEndpoint = aws.String(endpoint) })
	adapter.client = miniStackEC2{ec2API: client}
	return adapter, client
}

type miniStackEC2 struct{ ec2API }

func (client miniStackEC2) RunInstances(ctx context.Context, input *ec2.RunInstancesInput, options ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	output, err := client.ec2API.RunInstances(ctx, input, options...)
	if output != nil {
		for index := range output.Instances {
			addMiniStackPrivateIPv4(&output.Instances[index])
		}
	}
	return output, err
}

func (client miniStackEC2) DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, options ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	output, err := client.ec2API.DescribeInstances(ctx, input, options...)
	if output != nil {
		for reservationIndex := range output.Reservations {
			for instanceIndex := range output.Reservations[reservationIndex].Instances {
				addMiniStackPrivateIPv4(&output.Reservations[reservationIndex].Instances[instanceIndex])
			}
		}
	}
	return output, err
}

func addMiniStackPrivateIPv4(instance *types.Instance) {
	if instance.State != nil && instance.State.Name == types.InstanceStateNameRunning {
		instance.PrivateIpAddress = aws.String("10.0.0.4")
	}
}

func setMiniStackCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
}

func describeVolumes(t *testing.T, client *ec2.Client) []types.Volume {
	t.Helper()
	var volumes []types.Volume
	paginator := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(t.Context())
		if err != nil {
			t.Fatalf("DescribeVolumes(): %v", err)
		}
		volumes = append(volumes, page.Volumes...)
	}
	return volumes
}

func assertProviderError(t *testing.T, err error, code provider.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", code)
	}
	var providerError *provider.Error
	if !errors.As(err, &providerError) || providerError.Code != code {
		t.Fatalf("error = %T %v, want provider error %s", err, err, code)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("provider error exposes transport details: %q", err)
	}
}
