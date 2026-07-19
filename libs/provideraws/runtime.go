package provideraws

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	runtimeResource      = "runtime"
	systemVolumeResource = "system-volume"
)

var _ provider.RuntimeProvider = (*Provider)(nil)
var _ provider.RuntimeDataVolumeAttachmentObserver = (*Provider)(nil)

func (adapter *Provider) EnsureRuntime(ctx context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	instanceType, err := adapter.validateEnsureRuntimeRequest(request)
	if err != nil {
		return provider.Runtime{}, err
	}
	instances, err := adapter.findRuntimes(ctx, request.RuntimeID)
	if err != nil {
		return provider.Runtime{}, containError("describe Runtime", err)
	}
	if len(instances) > 1 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime identity is not unique", nil)
	}
	if len(instances) == 1 {
		instance := instances[0]
		if err := adapter.validateRuntime(request.RuntimeSpec, instance); err != nil {
			return provider.Runtime{}, err
		}
		observation, err := runtimeObservation(request.RuntimeSpec, instance)
		observation.SystemVolumeProviderID = ""
		if err != nil || observation.State == provider.RuntimeStateTerminated {
			return observation, err
		}
		if err := adapter.validateSoleActiveRuntime(ctx, request.EnvironmentID, observation.ProviderID); err != nil {
			return provider.Runtime{}, err
		}
		return observation, nil
	}
	active, err := adapter.findActiveRuntimes(ctx, request.EnvironmentID)
	if err != nil {
		return provider.Runtime{}, containError("describe current Runtimes", err)
	}
	if len(active) > 1 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Environment has multiple current Runtimes", nil)
	}
	if len(active) == 1 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodePlacementConflict, "Environment already has a current Runtime", nil)
	}
	volume, err := adapter.dataVolume(ctx, request.RuntimeSpec)
	if err != nil {
		return provider.Runtime{}, err
	}
	if len(volume.Attachments) != 0 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is already attached", nil)
	}
	output, err := adapter.client.RunInstances(ctx, adapter.runtimeLaunchInput(request, instanceType))
	if err != nil {
		return provider.Runtime{}, containError("create Runtime", err)
	}
	if len(output.Instances) != 1 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "provider returned an invalid Runtime allocation", nil)
	}
	observation, err := runtimeObservation(request.RuntimeSpec, output.Instances[0])
	observation.SystemVolumeProviderID = ""
	return observation, err
}

// EnsureRuntimeDataVolumeAttachment idempotently attaches the persistent Data
// Volume to an already-inventoried Runtime. Allocation deliberately lives in
// EnsureRuntime so callers can durably record provider identity before this
// second side effect begins.
func (adapter *Provider) EnsureRuntimeDataVolumeAttachment(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	observation, err := adapter.ObserveRuntime(ctx, request)
	if err != nil {
		return provider.Runtime{}, err
	}
	volume, err := adapter.dataVolume(ctx, request.RuntimeSpec)
	if err != nil {
		return provider.Runtime{}, err
	}
	if err := adapter.ensureDataVolumeAttachment(ctx, request.DataVolumeProviderID, types.Instance{InstanceId: aws.String(request.ProviderID)}, volume); err != nil {
		return provider.Runtime{}, err
	}
	return adapter.withSystemVolumeIdentity(ctx, observation)
}

// withSystemVolumeIdentity resolves the replaceable system volume's provider
// identity once it is observable; allocation-only EnsureRuntime deliberately
// returns before this so callers can persist the Runtime identity first.
func (adapter *Provider) withSystemVolumeIdentity(ctx context.Context, observation provider.Runtime) (provider.Runtime, error) {
	if observation.SystemVolumeProviderID != "" {
		return observation, nil
	}
	input := &ec2.DescribeVolumesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + tagRuntimeID), Values: []string{observation.RuntimeID}},
		{Name: aws.String("tag:" + tagResource), Values: []string{systemVolumeResource}},
	}}
	var volumes []types.Volume
	for {
		output, err := adapter.client.DescribeVolumes(ctx, input)
		if err != nil {
			return provider.Runtime{}, containError("describe Runtime system volume", err)
		}
		volumes = append(volumes, output.Volumes...)
		if output.NextToken == nil || aws.ToString(output.NextToken) == "" {
			break
		}
		input.NextToken = output.NextToken
	}
	if len(volumes) == 0 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeUnavailable, "Runtime system volume identity is not observable yet", nil)
	}
	if len(volumes) != 1 {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime system volume identity is not unique", nil)
	}
	volume := volumes[0]
	tags := tagValues(volume.Tags)
	wantTags := map[string]string{
		tagEnvironment: adapter.environment, tagEnvironmentID: observation.EnvironmentID,
		tagManagedBy: managedByValue, tagRegion: observation.Region, tagResource: systemVolumeResource,
		tagRuntimeID: observation.RuntimeID, tagRuntimeSequence: strconv.FormatInt(observation.Sequence, 10),
		tagRuntimePreset: observation.RuntimePreset, tagImageVersion: observation.ImageVersion,
		tagDataVolumeID: observation.DataVolumeProviderID,
	}
	for key, value := range wantTags {
		if tags[key] != value {
			return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, fmt.Sprintf("Runtime system volume ownership tag %q diverged", key), nil)
		}
	}
	providerID := aws.ToString(volume.VolumeId)
	if tags[tagOperationID] == "" || providerID == "" || aws.ToString(volume.AvailabilityZone) != observation.AvailabilityZone ||
		!aws.ToBool(volume.Encrypted) || volume.VolumeType != types.VolumeTypeGp3 || aws.ToInt32(volume.Size) != adapter.runtime.SystemVolumeGiB {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime system volume configuration diverged", nil)
	}
	observation.SystemVolumeProviderID = providerID
	return observation, nil
}

func (adapter *Provider) StartRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	observation, err := adapter.ObserveRuntime(ctx, request)
	if err != nil {
		return provider.Runtime{}, err
	}
	switch observation.State {
	case provider.RuntimeStatePending, provider.RuntimeStateRunning:
		return observation, nil
	case provider.RuntimeStateStopped:
		if _, err := adapter.client.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{request.ProviderID}}); err != nil {
			return provider.Runtime{}, containError("start Runtime", err)
		}
		observation.State = provider.RuntimeStatePending
		observation.PrivateIPv4 = ""
		return observation, nil
	case provider.RuntimeStateStopping:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeUnavailable, "Runtime is still stopping", nil)
	case provider.RuntimeStateTerminated:
		return observation, nil
	default:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "retired Runtime cannot start", nil)
	}
}

func (adapter *Provider) StopRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	observation, err := adapter.ObserveRuntime(ctx, request)
	if err != nil {
		return provider.Runtime{}, err
	}
	switch observation.State {
	case provider.RuntimeStateStopped, provider.RuntimeStateStopping:
		return observation, nil
	case provider.RuntimeStateRunning:
		if _, err := adapter.client.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{request.ProviderID}}); err != nil {
			return provider.Runtime{}, containError("stop Runtime", err)
		}
		return adapter.ObserveRuntime(ctx, request)
	case provider.RuntimeStatePending:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeUnavailable, "Runtime is still pending", nil)
	case provider.RuntimeStateTerminated:
		return observation, nil
	default:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "retired Runtime cannot stop", nil)
	}
}

func (adapter *Provider) RetireRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	observation, err := adapter.ObserveRuntime(ctx, request)
	if err != nil {
		return provider.Runtime{}, err
	}
	if observation.State == provider.RuntimeStateTerminated {
		return observation, nil
	}
	if observation.State == provider.RuntimeStateStopping {
		// Termination was already accepted. The workflow's shared durable-deadline
		// poller owns the remaining wait to terminated.
		return observation, nil
	}
	if observation.State != provider.RuntimeStateStopped {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeInvalidRequest, "Runtime must be stopped before retirement", nil)
	}
	volume, err := adapter.dataVolume(ctx, request.RuntimeSpec)
	if err != nil {
		return provider.Runtime{}, err
	}
	switch len(volume.Attachments) {
	case 0:
	case 1:
		attachment := volume.Attachments[0]
		if aws.ToString(attachment.InstanceId) != request.ProviderID {
			return provider.Runtime{}, provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is attached to another Runtime", nil)
		}
		if attachment.State != types.VolumeAttachmentStateAttached && attachment.State != types.VolumeAttachmentStateDetaching {
			return provider.Runtime{}, provider.NewError(provider.ErrorCodeUnavailable, "Data Volume attachment is not ready for retirement", nil)
		}
		if aws.ToBool(attachment.DeleteOnTermination) {
			if attachment.State == types.VolumeAttachmentStateDetaching {
				return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "detaching Data Volume attachment is still configured for deletion", nil)
			}
			if _, err := adapter.client.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
				InstanceId: aws.String(request.ProviderID),
				BlockDeviceMappings: []types.InstanceBlockDeviceMappingSpecification{{
					DeviceName: attachment.Device,
					Ebs: &types.EbsInstanceBlockDeviceSpecification{
						DeleteOnTermination: aws.Bool(false), VolumeId: aws.String(request.DataVolumeProviderID),
					},
				}},
			}); err != nil {
				return provider.Runtime{}, containError("make Data Volume attachment persistent", err)
			}
			if err := adapter.waitForDataVolumeAttachment(ctx, request, func(attachments []types.VolumeAttachment) (bool, error) {
				if len(attachments) != 1 {
					return false, nil
				}
				if aws.ToString(attachments[0].InstanceId) != request.ProviderID {
					return false, provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is attached to another Runtime", nil)
				}
				return !aws.ToBool(attachments[0].DeleteOnTermination), nil
			}, "Data Volume attachment did not become persistent before retirement"); err != nil {
				return provider.Runtime{}, err
			}
		}
		if attachment.State == types.VolumeAttachmentStateAttached {
			if _, err := adapter.client.DetachVolume(ctx, &ec2.DetachVolumeInput{InstanceId: aws.String(request.ProviderID), VolumeId: aws.String(request.DataVolumeProviderID)}); err != nil {
				return provider.Runtime{}, containError("detach Data Volume", err)
			}
		}
		if err := adapter.waitForDataVolumeAttachment(ctx, request, func(attachments []types.VolumeAttachment) (bool, error) {
			switch len(attachments) {
			case 0:
				return true, nil
			case 1:
				if aws.ToString(attachments[0].InstanceId) != request.ProviderID {
					return false, provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is attached to another Runtime", nil)
				}
				return false, nil
			default:
				return false, provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume has multiple attachments", nil)
			}
		}, "Data Volume remained attached during retirement"); err != nil {
			return provider.Runtime{}, err
		}
	default:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume has multiple attachments", nil)
	}
	if _, err := adapter.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{request.ProviderID}}); err != nil {
		return provider.Runtime{}, containError("retire Runtime", err)
	}
	return adapter.ObserveRuntime(ctx, request)
}

func (adapter *Provider) waitForDataVolumeAttachment(
	ctx context.Context,
	request provider.RuntimeLifecycleRequest,
	ready func([]types.VolumeAttachment) (bool, error),
	timeoutMessage string,
) error {
	interval := adapter.runtimePollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	timeout := adapter.runtimePollTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		volume, err := adapter.dataVolume(pollCtx, request.RuntimeSpec)
		if err != nil {
			if pollCtx.Err() != nil && ctx.Err() == nil {
				return provider.NewError(provider.ErrorCodeResourceDiverged, timeoutMessage, pollCtx.Err())
			}
			return err
		}
		complete, err := ready(volume.Attachments)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return provider.NewError(provider.ErrorCodeResourceDiverged, timeoutMessage, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

func (adapter *Provider) ObserveRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if _, err := adapter.validateRuntimeSpec(request.RuntimeSpec); err != nil {
		return provider.Runtime{}, err
	}
	if strings.TrimSpace(request.ProviderID) == "" {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeInvalidRequest, "provider Runtime identity is required", nil)
	}
	output, err := adapter.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{request.ProviderID}})
	if err != nil {
		return provider.Runtime{}, containError("observe Runtime", err)
	}
	instances := instancesFromReservations(output.Reservations)
	if len(instances) != 1 || aws.ToString(instances[0].InstanceId) != request.ProviderID {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "provider Runtime identity is not unique", nil)
	}
	if err := adapter.validateRuntime(request.RuntimeSpec, instances[0]); err != nil {
		return provider.Runtime{}, err
	}
	return runtimeObservation(request.RuntimeSpec, instances[0])
}

func (adapter *Provider) ObserveRuntimeDataVolumeAttachment(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.RuntimeDataVolumeAttachment, error) {
	if _, err := adapter.validateRuntimeSpec(request.RuntimeSpec); err != nil {
		return provider.RuntimeDataVolumeAttachment{}, err
	}
	if strings.TrimSpace(request.ProviderID) == "" {
		return provider.RuntimeDataVolumeAttachment{}, provider.NewError(provider.ErrorCodeInvalidRequest, "provider Runtime identity is required", nil)
	}
	volume, err := adapter.dataVolume(ctx, request.RuntimeSpec)
	if err != nil {
		return provider.RuntimeDataVolumeAttachment{}, err
	}
	observation := provider.RuntimeDataVolumeAttachment{DataVolumeProviderID: request.DataVolumeProviderID}
	switch len(volume.Attachments) {
	case 0:
		return observation, nil
	case 1:
		attachment := volume.Attachments[0]
		if aws.ToString(attachment.InstanceId) != request.ProviderID {
			return provider.RuntimeDataVolumeAttachment{}, provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is attached to another Runtime", nil)
		}
		if aws.ToBool(attachment.DeleteOnTermination) {
			return provider.RuntimeDataVolumeAttachment{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume attachment is not persistent", nil)
		}
		observation.RuntimeProviderID = request.ProviderID
		observation.Attached = true
		observation.ReadWrite = true
		return observation, nil
	default:
		return provider.RuntimeDataVolumeAttachment{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume has multiple attachments", nil)
	}
}

func (adapter *Provider) ensureDataVolumeAttachment(ctx context.Context, volumeID string, instance types.Instance, volume types.Volume) error {
	switch len(volume.Attachments) {
	case 0:
		attachment, err := adapter.client.AttachVolume(ctx, &ec2.AttachVolumeInput{
			Device: aws.String("/dev/sdf"), InstanceId: instance.InstanceId, VolumeId: aws.String(volumeID),
		})
		if err != nil {
			return containError("attach Data Volume", err)
		}
		if aws.ToBool(attachment.DeleteOnTermination) {
			return provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume attachment is not persistent", nil)
		}
		return nil
	case 1:
		attachment := volume.Attachments[0]
		if aws.ToString(attachment.InstanceId) != aws.ToString(instance.InstanceId) || aws.ToString(attachment.Device) != "/dev/sdf" {
			return provider.NewError(provider.ErrorCodePlacementConflict, "Data Volume is attached to another Runtime", nil)
		}
		if aws.ToBool(attachment.DeleteOnTermination) {
			return provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume attachment is not persistent", nil)
		}
		return nil
	default:
		return provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume has multiple attachments", nil)
	}
}

func (adapter *Provider) validateRuntime(spec provider.RuntimeSpec, instance types.Instance) error {
	tags := tagValues(instance.Tags)
	wantTags := map[string]string{
		tagEnvironment: adapter.environment, tagEnvironmentID: spec.EnvironmentID, tagManagedBy: managedByValue,
		tagRegion: spec.Region, tagResource: runtimeResource, tagRuntimeID: spec.RuntimeID,
		tagRuntimeSequence: strconv.FormatInt(spec.Sequence, 10), tagRuntimePreset: spec.RuntimePreset,
		tagImageVersion: spec.ImageVersion, tagDataVolumeID: spec.DataVolumeProviderID,
	}
	for key, value := range wantTags {
		if tags[key] != value {
			return provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime ownership is not valid", nil)
		}
	}
	instanceType := adapter.runtime.Presets[spec.RuntimePreset]
	if tags[tagOperationID] == "" || aws.ToString(instance.ImageId) != adapter.runtime.AMI || string(instance.InstanceType) != instanceType || instance.Placement == nil || aws.ToString(instance.Placement.AvailabilityZone) != spec.AvailabilityZone {
		return provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime configuration diverged", nil)
	}
	return nil
}

func (adapter *Provider) validateEnsureRuntimeRequest(request provider.EnsureRuntimeRequest) (string, error) {
	if strings.TrimSpace(request.OperationID) == "" {
		return "", provider.NewError(provider.ErrorCodeInvalidRequest, "Operation is required", nil)
	}
	return adapter.validateRuntimeSpec(request.RuntimeSpec)
}

func (adapter *Provider) validateRuntimeSpec(spec provider.RuntimeSpec) (string, error) {
	if spec.Sequence < 1 || strings.TrimSpace(spec.RuntimeID) == "" || strings.TrimSpace(spec.EnvironmentID) == "" || strings.TrimSpace(spec.Region) == "" || strings.TrimSpace(spec.AvailabilityZone) == "" || strings.TrimSpace(spec.RuntimePreset) == "" || strings.TrimSpace(spec.ImageVersion) == "" || strings.TrimSpace(spec.DataVolumeProviderID) == "" {
		return "", provider.NewError(provider.ErrorCodeInvalidRequest, "Runtime identity, sequence, placement, preset, image, and Data Volume are required", nil)
	}
	if spec.Region != adapter.region {
		return "", provider.NewError(provider.ErrorCodePlacementConflict, fmt.Sprintf("requested region %q does not match adapter region %q", spec.Region, adapter.region), nil)
	}
	instanceType := strings.TrimSpace(adapter.runtime.Presets[spec.RuntimePreset])
	if strings.TrimSpace(adapter.runtime.AMI) == "" || instanceType == "" || strings.TrimSpace(adapter.runtime.SubnetID) == "" || strings.TrimSpace(adapter.runtime.SecurityGroupID) == "" || adapter.runtime.SystemVolumeGiB < 1 {
		return "", provider.NewError(provider.ErrorCodeInvalidRequest, "Runtime adapter configuration is incomplete", nil)
	}
	return instanceType, nil
}

func (adapter *Provider) findRuntimes(ctx context.Context, runtimeID string) ([]types.Instance, error) {
	return adapter.describeRuntimes(ctx, []types.Filter{
		{Name: aws.String("tag:" + tagRuntimeID), Values: []string{runtimeID}},
		{Name: aws.String("tag:" + tagResource), Values: []string{runtimeResource}},
	})
}

func (adapter *Provider) findActiveRuntimes(ctx context.Context, environmentID string) ([]types.Instance, error) {
	return adapter.describeRuntimes(ctx, []types.Filter{
		{Name: aws.String("tag:" + tagEnvironmentID), Values: []string{environmentID}},
		{Name: aws.String("tag:" + tagResource), Values: []string{runtimeResource}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	})
}

func (adapter *Provider) validateSoleActiveRuntime(ctx context.Context, environmentID, providerID string) error {
	active, err := adapter.findActiveRuntimes(ctx, environmentID)
	if err != nil {
		return containError("describe current Runtimes", err)
	}
	if len(active) != 1 || aws.ToString(active[0].InstanceId) != providerID {
		return provider.NewError(provider.ErrorCodeResourceDiverged, "Environment current Runtime inventory diverged", nil)
	}
	return nil
}

func (adapter *Provider) describeRuntimes(ctx context.Context, filters []types.Filter) ([]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{Filters: filters}
	var instances []types.Instance
	for {
		output, err := adapter.client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instancesFromReservations(output.Reservations)...)
		if output.NextToken == nil || *output.NextToken == "" {
			return instances, nil
		}
		input.NextToken = output.NextToken
	}
}

func instancesFromReservations(reservations []types.Reservation) []types.Instance {
	var instances []types.Instance
	for _, reservation := range reservations {
		instances = append(instances, reservation.Instances...)
	}
	return instances
}

func (adapter *Provider) dataVolume(ctx context.Context, spec provider.RuntimeSpec) (types.Volume, error) {
	output, err := adapter.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{spec.DataVolumeProviderID}})
	if err != nil {
		return types.Volume{}, containError("describe Data Volume", err)
	}
	if len(output.Volumes) != 1 {
		return types.Volume{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Data Volume does not exist uniquely", nil)
	}
	volume := output.Volumes[0]
	_, err = adapter.ownedDataVolume(provider.EnsureDataVolumeRequest{
		EnvironmentID: spec.EnvironmentID, Region: spec.Region, AvailabilityZone: spec.AvailabilityZone,
	}, volume)
	return volume, err
}

func (adapter *Provider) runtimeLaunchInput(request provider.EnsureRuntimeRequest, instanceType string) *ec2.RunInstancesInput {
	instanceTags := append(adapter.ownershipTags(request.EnvironmentID, request.OperationID, request.Region, runtimeResource), runtimeTags(request.RuntimeSpec)...)
	systemVolumeTags := append(adapter.ownershipTags(request.EnvironmentID, request.OperationID, request.Region, systemVolumeResource), runtimeTags(request.RuntimeSpec)...)
	return &ec2.RunInstancesInput{
		ImageId: aws.String(adapter.runtime.AMI), InstanceType: types.InstanceType(instanceType), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		ClientToken: aws.String(providerToken(runtimeResource, request.RuntimeID)),
		Placement:   &types.Placement{AvailabilityZone: aws.String(request.AvailabilityZone)},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			AssociatePublicIpAddress: aws.Bool(false), DeleteOnTermination: aws.Bool(true), DeviceIndex: aws.Int32(0),
			SubnetId: aws.String(adapter.runtime.SubnetID), Groups: []string{adapter.runtime.SecurityGroupID},
		}},
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpTokens: types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"), Ebs: &types.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true), VolumeSize: aws.Int32(adapter.runtime.SystemVolumeGiB), VolumeType: types.VolumeTypeGp3,
			},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: instanceTags},
			{ResourceType: types.ResourceTypeVolume, Tags: systemVolumeTags},
		},
	}
}

func runtimeTags(spec provider.RuntimeSpec) []types.Tag {
	values := [...]struct{ key, value string }{
		{tagRuntimeID, spec.RuntimeID}, {tagRuntimeSequence, strconv.FormatInt(spec.Sequence, 10)},
		{tagRuntimePreset, spec.RuntimePreset}, {tagImageVersion, spec.ImageVersion}, {tagDataVolumeID, spec.DataVolumeProviderID},
	}
	tags := make([]types.Tag, 0, len(values))
	for _, value := range values {
		tags = append(tags, types.Tag{Key: aws.String(value.key), Value: aws.String(value.value)})
	}
	return tags
}

func runtimeObservation(spec provider.RuntimeSpec, instance types.Instance) (provider.Runtime, error) {
	state := provider.RuntimeState("")
	if instance.State != nil {
		if instance.State.Name == types.InstanceStateNameShuttingDown {
			state = provider.RuntimeStateStopping
		} else {
			state = provider.RuntimeState(instance.State.Name)
		}
	}
	switch state {
	case provider.RuntimeStatePending, provider.RuntimeStateRunning, provider.RuntimeStateStopping, provider.RuntimeStateStopped, provider.RuntimeStateTerminated:
	default:
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime has an unsupported provider state", nil)
	}
	providerID := aws.ToString(instance.InstanceId)
	if providerID == "" {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime has no provider identity", nil)
	}
	privateIPv4 := ""
	if state == provider.RuntimeStateRunning {
		privateIPv4 = aws.ToString(instance.PrivateIpAddress)
		address, err := netip.ParseAddr(privateIPv4)
		if err != nil || !address.Is4() || !address.IsPrivate() {
			return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "running Runtime has no private IPv4 route", nil)
		}
	}
	systemVolumeProviderID := ""
	for _, mapping := range instance.BlockDeviceMappings {
		if aws.ToString(mapping.DeviceName) == "/dev/sda1" && mapping.Ebs != nil {
			systemVolumeProviderID = aws.ToString(mapping.Ebs.VolumeId)
			break
		}
	}
	return provider.Runtime{
		RuntimeSpec: spec, Provider: "aws", ProviderID: providerID, SystemVolumeProviderID: systemVolumeProviderID,
		PrivateIPv4: privateIPv4, State: state,
	}, nil
}
