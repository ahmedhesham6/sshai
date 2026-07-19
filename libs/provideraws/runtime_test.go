package provideraws

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/provider/providertest"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type recordingEC2 struct {
	runInput    *ec2.RunInstancesInput
	attachInput *ec2.AttachVolumeInput
	runErr      error
	attachErr   error
}

type divergentRuntimeEC2 struct {
	*recordingEC2
	describeCalls int
}

type acceptedStartEC2 struct {
	*recordingEC2
	describeCalls int
	startCalls    int
}

type terminatedRuntimeEC2 struct {
	*recordingEC2
	describeCalls int
}

func (client *divergentRuntimeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	client.describeCalls++
	current := ownedInstanceForTest("i-current", "runtime-1")
	instances := []types.Instance{current}
	if client.describeCalls > 1 {
		instances = append(instances, ownedInstanceForTest("i-extra", "runtime-extra"))
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: instances}}}, nil
}

func (client *acceptedStartEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	client.describeCalls++
	if client.describeCalls > 1 {
		return nil, errors.New("unexpected follow-up Runtime observation")
	}
	instance := ownedInstanceForTest("i-runtime", "runtime-1")
	instance.State.Name = types.InstanceStateNameStopped
	instance.PrivateIpAddress = nil
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance}}}}, nil
}

func (client *acceptedStartEC2) StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	client.startCalls++
	return &ec2.StartInstancesOutput{}, nil
}

func (client *terminatedRuntimeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	client.describeCalls++
	instance := ownedInstanceForTest("i-runtime", "runtime-1")
	instance.State.Name = types.InstanceStateNameTerminated
	instance.PrivateIpAddress = nil
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance}}}}, nil
}

func (client *recordingEC2) DetachVolume(context.Context, *ec2.DetachVolumeInput, ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error) {
	panic("unexpected DetachVolume")
}

func (client *recordingEC2) StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	panic("unexpected StartInstances")
}

func (client *recordingEC2) StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	panic("unexpected StopInstances")
}

func (client *recordingEC2) TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	panic("unexpected TerminateInstances")
}

func (client *recordingEC2) CreateVolume(context.Context, *ec2.CreateVolumeInput, ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error) {
	panic("unexpected CreateVolume")
}

func (client *recordingEC2) DescribeVolumes(_ context.Context, _ *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []types.Volume{{
		AvailabilityZone: aws.String("us-east-1a"), Encrypted: aws.Bool(true),
		Size: aws.Int32(100), VolumeId: aws.String("vol-data"), VolumeType: types.VolumeTypeGp3,
		Tags: ownershipTagsForTest("environment-1", "operation-1", "data-volume"),
	}}}, nil
}

func (client *recordingEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if client.runInput != nil {
		instance := ownedInstanceForTest("i-runtime", "runtime-1")
		instance.State.Name = types.InstanceStateNamePending
		instance.PrivateIpAddress = nil
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance}}}}, nil
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (client *recordingEC2) RunInstances(_ context.Context, input *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	client.runInput = input
	if client.runErr != nil {
		return nil, client.runErr
	}
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{
		InstanceId: aws.String("i-runtime"), PrivateIpAddress: aws.String("10.0.0.4"),
		State: &types.InstanceState{Name: types.InstanceStateNamePending},
	}}}, nil
}

func (client *recordingEC2) AttachVolume(_ context.Context, input *ec2.AttachVolumeInput, _ ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error) {
	client.attachInput = input
	if client.attachErr != nil {
		return nil, client.attachErr
	}
	return &ec2.AttachVolumeOutput{DeleteOnTermination: aws.Bool(false)}, nil
}

func TestEnsureRuntimeInventoriesAllocationBeforeAttachingPersistentData(t *testing.T) {
	client := &recordingEC2{}
	adapter, err := newProvider(client, Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		Runtime: RuntimeConfig{
			AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"},
			SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
		},
	})
	if err != nil {
		t.Fatalf("newProvider(): %v", err)
	}

	runtime, err := adapter.EnsureRuntime(t.Context(), runtimeRequest("runtime-1", 1, "image-v1", "vol-data"))
	if err != nil {
		t.Fatalf("EnsureRuntime(): %v", err)
	}
	if runtime.ProviderID != "i-runtime" || runtime.PrivateIPv4 != "" || runtime.State != provider.RuntimeStatePending || runtime.RuntimeID != "runtime-1" {
		t.Fatalf("Runtime = %#v", runtime)
	}
	assertRuntimeLaunchInput(t, client.runInput)
	if client.attachInput != nil {
		t.Fatalf("EnsureRuntime attached Data Volume before allocation was inventoried: %#v", client.attachInput)
	}
	request := runtimeRequest("runtime-1", 1, "image-v1", "vol-data")
	if _, err := adapter.EnsureRuntimeDataVolumeAttachment(t.Context(), provider.RuntimeLifecycleRequest{RuntimeSpec: request.RuntimeSpec, ProviderID: runtime.ProviderID}); err != nil {
		t.Fatalf("EnsureRuntimeDataVolumeAttachment(): %v", err)
	}
	if client.attachInput == nil || aws.ToString(client.attachInput.InstanceId) != "i-runtime" || aws.ToString(client.attachInput.VolumeId) != "vol-data" || aws.ToString(client.attachInput.Device) != "/dev/sdf" {
		t.Fatalf("AttachVolume input = %#v", client.attachInput)
	}
}

func TestRuntimeAttachmentFailureDoesNotHideAllocatedIdentity(t *testing.T) {
	client := &recordingEC2{attachErr: &smithy.GenericAPIError{Code: "InternalError", Message: "attach failed"}}
	adapter, err := newProvider(client, Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		Runtime: RuntimeConfig{AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"}, SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 30},
	})
	if err != nil {
		t.Fatalf("newProvider(): %v", err)
	}
	request := runtimeRequest("runtime-1", 1, "image-v1", "vol-data")
	runtime, err := adapter.EnsureRuntime(t.Context(), request)
	if err != nil || runtime.ProviderID != "i-runtime" {
		t.Fatalf("allocated Runtime = %#v error:%v", runtime, err)
	}
	_, err = adapter.EnsureRuntimeDataVolumeAttachment(t.Context(), provider.RuntimeLifecycleRequest{RuntimeSpec: request.RuntimeSpec, ProviderID: runtime.ProviderID})
	if err == nil || runtime.ProviderID == "" {
		t.Fatalf("attachment error = %v after allocation %#v", err, runtime)
	}
}

func TestStartRuntimeReturnsAcceptedPendingWithoutFollowUpObservation(t *testing.T) {
	client := &acceptedStartEC2{recordingEC2: &recordingEC2{}}
	adapter, err := newProvider(client, Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		Runtime: RuntimeConfig{
			AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"},
			SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
		},
	})
	if err != nil {
		t.Fatalf("newProvider(): %v", err)
	}
	request := runtimeRequest("runtime-1", 1, "image-v1", "vol-data")
	started, err := adapter.StartRuntime(t.Context(), provider.RuntimeLifecycleRequest{
		RuntimeSpec: request.RuntimeSpec, ProviderID: "i-runtime",
	})
	if err != nil {
		t.Fatalf("StartRuntime(): %v", err)
	}
	if client.startCalls != 1 || client.describeCalls != 1 || started.State != provider.RuntimeStatePending || started.PrivateIPv4 != "" {
		t.Fatalf("accepted start calls/observation = %d/%d %#v", client.startCalls, client.describeCalls, started)
	}
}

func TestRuntimeLifecycleReturnsPreTerminatedObservation(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Provider, context.Context, provider.RuntimeLifecycleRequest) (provider.Runtime, error)
	}{
		{name: "start", invoke: (*Provider).StartRuntime},
		{name: "stop", invoke: (*Provider).StopRuntime},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &terminatedRuntimeEC2{recordingEC2: &recordingEC2{}}
			adapter, err := newProvider(client, Config{
				Region: "us-east-1", Environment: "development", SizeGiB: 100,
				Runtime: RuntimeConfig{
					AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"},
					SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
				},
			})
			if err != nil {
				t.Fatalf("newProvider(): %v", err)
			}
			request := runtimeRequest("runtime-1", 1, "image-v1", "vol-data")
			observation, err := test.invoke(adapter, t.Context(), provider.RuntimeLifecycleRequest{RuntimeSpec: request.RuntimeSpec, ProviderID: "i-runtime"})
			if err != nil {
				t.Fatalf("%s pre-terminated Runtime: %v", test.name, err)
			}
			if client.describeCalls != 1 || observation.State != provider.RuntimeStateTerminated || observation.ProviderID != "i-runtime" || observation.RuntimeSpec != request.RuntimeSpec {
				t.Fatalf("%s calls/observation = %d/%#v", test.name, client.describeCalls, observation)
			}
		})
	}
}

func TestRuntimeProviderConformance(t *testing.T) {
	providertest.RunRuntimeLifecycle(t, func(t *testing.T) providertest.RuntimeHarness {
		adapter, ec2Client := startAdapter(t)
		volume, err := adapter.EnsureDataVolume(t.Context(), provider.EnsureDataVolumeRequest{
			EnvironmentID: "environment-1", OperationID: "operation-1",
			Region: "us-east-1", AvailabilityZone: "us-east-1a",
		})
		if err != nil {
			t.Fatalf("EnsureDataVolume(): %v", err)
		}
		return providertest.RuntimeHarness{
			Adapter: adapter,
			Request: runtimeRequest("runtime-1", 1, "image-v1", volume.ProviderID),
			AssertDataVolumePreserved: func(t *testing.T) {
				volumes := describeVolumes(t, ec2Client)
				if len(volumes) != 1 || len(volumes[0].Attachments) != 0 {
					t.Fatalf("retired Runtime Data Volume = %#v", volumes)
				}
			},
		}
	})
}

func TestRuntimeReplacementRetiresOldBeforeReattachingPersistentData(t *testing.T) {
	adapter, ec2Client := startAdapter(t)
	volume, err := adapter.EnsureDataVolume(t.Context(), provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	})
	if err != nil {
		t.Fatalf("EnsureDataVolume(): %v", err)
	}
	oldRequest := runtimeRequest("runtime-1", 1, "image-v1", volume.ProviderID)
	old, err := adapter.EnsureRuntime(t.Context(), oldRequest)
	if err != nil {
		t.Fatalf("EnsureRuntime(): %v", err)
	}
	oldLifecycle := provider.RuntimeLifecycleRequest{RuntimeSpec: oldRequest.RuntimeSpec, ProviderID: old.ProviderID}
	if _, err := adapter.EnsureRuntimeDataVolumeAttachment(t.Context(), oldLifecycle); err != nil {
		t.Fatalf("attach old Runtime Data Volume: %v", err)
	}
	newRequest := runtimeRequest("runtime-2", 2, "image-v2", volume.ProviderID)
	if _, err := adapter.EnsureRuntime(t.Context(), newRequest); err == nil {
		t.Fatal("EnsureRuntime() attached replacement before retiring old Runtime")
	}
	lifecycle := oldLifecycle
	if _, err := adapter.StartRuntime(t.Context(), lifecycle); err != nil {
		t.Fatalf("StartRuntime(): %v", err)
	}
	if _, err := adapter.StopRuntime(t.Context(), lifecycle); err != nil {
		t.Fatalf("StopRuntime(): %v", err)
	}
	if _, err := adapter.RetireRuntime(t.Context(), lifecycle); err != nil {
		t.Fatalf("RetireRuntime(): %v", err)
	}
	replacement, err := adapter.EnsureRuntime(t.Context(), newRequest)
	if err != nil {
		t.Fatalf("ensure replacement Runtime: %v", err)
	}
	if replacement.ProviderID == old.ProviderID || replacement.RuntimeID != "runtime-2" {
		t.Fatalf("replacement Runtime = %#v; old = %#v", replacement, old)
	}
	if _, err := adapter.EnsureRuntimeDataVolumeAttachment(t.Context(), provider.RuntimeLifecycleRequest{RuntimeSpec: newRequest.RuntimeSpec, ProviderID: replacement.ProviderID}); err != nil {
		t.Fatalf("attach replacement Runtime Data Volume: %v", err)
	}
	volumes := describeVolumes(t, ec2Client)
	if len(volumes) != 1 || len(volumes[0].Attachments) != 1 || aws.ToString(volumes[0].Attachments[0].InstanceId) != replacement.ProviderID || aws.ToBool(volumes[0].Attachments[0].DeleteOnTermination) {
		t.Fatalf("replacement Data Volume attachment = %#v", volumes)
	}
}

func TestEnsureRuntimeContainsCapacityFailure(t *testing.T) {
	client := &recordingEC2{runErr: &smithy.GenericAPIError{Code: "InsufficientInstanceCapacity", Message: "provider detail"}}
	adapter, err := newProvider(client, Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		Runtime: RuntimeConfig{
			AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"},
			SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
		},
	})
	if err != nil {
		t.Fatalf("newProvider(): %v", err)
	}
	_, err = adapter.EnsureRuntime(t.Context(), runtimeRequest("runtime-1", 1, "image-v1", "vol-data"))
	assertProviderError(t, err, provider.ErrorCodeCapacityUnavailable)
}

func TestEnsureRuntimeRejectsMultipleActiveRuntimesWhileReconcilingCurrent(t *testing.T) {
	client := &divergentRuntimeEC2{recordingEC2: &recordingEC2{}}
	adapter, err := newProvider(client, Config{
		Region: "us-east-1", Environment: "development", SizeGiB: 100,
		Runtime: RuntimeConfig{
			AMI: "ami-pinned", Presets: map[string]string{"standard": "m7i.xlarge"},
			SubnetID: "subnet-private", SecurityGroupID: "sg-runtime", SystemVolumeGiB: 24,
		},
	})
	if err != nil {
		t.Fatalf("newProvider(): %v", err)
	}
	_, err = adapter.EnsureRuntime(t.Context(), runtimeRequest("runtime-1", 1, "image-v1", "vol-data"))
	assertProviderError(t, err, provider.ErrorCodeResourceDiverged)
	if client.attachInput != nil {
		t.Fatalf("diverged inventory attached Data Volume: %#v", client.attachInput)
	}
}

func assertRuntimeLaunchInput(t *testing.T, input *ec2.RunInstancesInput) {
	t.Helper()
	if input == nil {
		t.Fatal("RunInstances was not called")
	}
	if aws.ToString(input.ImageId) != "ami-pinned" || input.InstanceType != types.InstanceType("m7i.xlarge") {
		t.Fatalf("image/type = %q/%q", aws.ToString(input.ImageId), input.InstanceType)
	}
	if aws.ToString(input.ClientToken) != "sshai-066a5f3ba19edfd1eea4da738d8455024fd3bf651c91e2bd67e0193b" {
		t.Fatalf("client token = %q", aws.ToString(input.ClientToken))
	}
	if input.Placement == nil || aws.ToString(input.Placement.AvailabilityZone) != "us-east-1a" {
		t.Fatalf("placement = %#v", input.Placement)
	}
	if len(input.NetworkInterfaces) != 1 {
		t.Fatalf("network interfaces = %#v", input.NetworkInterfaces)
	}
	network := input.NetworkInterfaces[0]
	if aws.ToBool(network.AssociatePublicIpAddress) || aws.ToInt32(network.DeviceIndex) != 0 || aws.ToString(network.SubnetId) != "subnet-private" || !slices.Equal(network.Groups, []string{"sg-runtime"}) {
		t.Fatalf("private network = %#v", network)
	}
	if input.MetadataOptions == nil || input.MetadataOptions.HttpTokens != types.HttpTokensStateRequired || input.MetadataOptions.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || aws.ToInt32(input.MetadataOptions.HttpPutResponseHopLimit) != 1 {
		t.Fatalf("metadata options = %#v", input.MetadataOptions)
	}
	if len(input.BlockDeviceMappings) != 1 {
		t.Fatalf("block devices = %#v", input.BlockDeviceMappings)
	}
	system := input.BlockDeviceMappings[0]
	if aws.ToString(system.DeviceName) != "/dev/sda1" || system.Ebs == nil || !aws.ToBool(system.Ebs.Encrypted) || !aws.ToBool(system.Ebs.DeleteOnTermination) || aws.ToInt32(system.Ebs.VolumeSize) != 24 || system.Ebs.VolumeType != types.VolumeTypeGp3 {
		t.Fatalf("system volume = %#v", system)
	}
	assertOwnershipTags(t, input.TagSpecifications, types.ResourceTypeInstance, "runtime")
	assertOwnershipTags(t, input.TagSpecifications, types.ResourceTypeVolume, "system-volume")
	assertRuntimeTags(t, input.TagSpecifications)
}

func assertRuntimeTags(t *testing.T, specifications []types.TagSpecification) {
	t.Helper()
	for _, specification := range specifications {
		if specification.ResourceType != types.ResourceTypeInstance {
			continue
		}
		got := tagValues(specification.Tags)
		if got[tagRuntimeID] != "runtime-1" || got[tagRuntimeSequence] != "1" || got[tagRuntimePreset] != "standard" || got[tagImageVersion] != "image-v1" || got[tagDataVolumeID] != "vol-data" {
			t.Fatalf("Runtime tags = %v", got)
		}
		return
	}
	t.Fatal("missing Runtime instance tags")
}

func runtimeRequest(runtimeID string, sequence int64, imageVersion, dataVolumeID string) provider.EnsureRuntimeRequest {
	return provider.EnsureRuntimeRequest{
		RuntimeSpec: provider.RuntimeSpec{
			RuntimeID: runtimeID, EnvironmentID: "environment-1", Sequence: sequence,
			Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
			ImageVersion: imageVersion, DataVolumeProviderID: dataVolumeID,
		},
		OperationID: "operation-2",
	}
}

func ownedInstanceForTest(providerID, runtimeID string) types.Instance {
	request := runtimeRequest(runtimeID, 1, "image-v1", "vol-data")
	tags := append(ownershipTagsForTest(request.EnvironmentID, request.OperationID, runtimeResource), runtimeTags(request.RuntimeSpec)...)
	return types.Instance{
		InstanceId: aws.String(providerID), ImageId: aws.String("ami-pinned"), InstanceType: types.InstanceType("m7i.xlarge"),
		Placement: &types.Placement{AvailabilityZone: aws.String("us-east-1a")}, PrivateIpAddress: aws.String("10.0.0.4"),
		State: &types.InstanceState{Name: types.InstanceStateNameRunning}, Tags: tags,
	}
}

func assertOwnershipTags(t *testing.T, specifications []types.TagSpecification, resourceType types.ResourceType, resource string) {
	t.Helper()
	for _, specification := range specifications {
		if specification.ResourceType == resourceType {
			got := make(map[string]string, len(specification.Tags))
			for _, tag := range specification.Tags {
				got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
			}
			if got[tagEnvironment] == "development" && got[tagEnvironmentID] == "environment-1" && got[tagManagedBy] == "sshai" && got[tagOperationID] == "operation-2" && got[tagRegion] == "us-east-1" && got[tagResource] == resource {
				return
			}
			t.Fatalf("%s tags = %v", resourceType, got)
		}
	}
	t.Fatalf("missing %s tags", resourceType)
}

func ownershipTagsForTest(environmentID, operationID, resource string) []types.Tag {
	values := map[string]string{
		tagEnvironment: "development", tagEnvironmentID: environmentID,
		tagManagedBy: "sshai", tagOperationID: operationID,
		tagRegion: "us-east-1", tagResource: resource,
	}
	tags := make([]types.Tag, 0, len(values))
	for key, value := range values {
		tags = append(tags, types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	return tags
}

func describeInstances(t *testing.T, client *ec2.Client) []types.Instance {
	t.Helper()
	var instances []types.Instance
	paginator := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(t.Context())
		if err != nil {
			t.Fatalf("DescribeInstances(): %v", err)
		}
		for _, reservation := range page.Reservations {
			instances = append(instances, reservation.Instances...)
		}
	}
	return instances
}
