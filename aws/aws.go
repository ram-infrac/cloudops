package aws

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/opsworks"
	sh "github.com/codeskyblue/go-sh"
	"github.com/libopenstorage/cloudops"
	"github.com/libopenstorage/cloudops/backoff"
	"github.com/libopenstorage/cloudops/pkg/exec"
	"github.com/libopenstorage/cloudops/unsupported"
	awscredentials "github.com/libopenstorage/secrets/aws/credentials"
	"github.com/portworx/sched-ops/task"
	"github.com/sirupsen/logrus"
)

const (
	awsDevicePrefix      = "/dev/sd"
	awsDevicePrefixWithX = "/dev/xvd"
	awsDevicePrefixWithH = "/dev/hd"
	awsDevicePrefixNvme  = "/dev/nvme"
)

type awsOps struct {
	cloudops.Compute
	instanceType string
	instance     string
	zone         string
	region       string
	ec2          *ec2.EC2
	autoscaling  *autoscaling.AutoScaling
	mutex        sync.Mutex
}

var (
	// ErrAWSEnvNotAvailable is the error type when aws credentials are not set
	ErrAWSEnvNotAvailable = fmt.Errorf("AWS credentials are not set in environment")
	nvmeCmd               = exec.Which("nvme")
)

// NewClient creates a new cloud operations client for AWS
func NewClient() (cloudops.Ops, error) {
	zone, instanceID, instanceType, err := getInfoFromMetadata()
	if err != nil {
		// try to get it from env
		zone, instanceID, instanceType, err = getInfoFromEnv()
		if err != nil {
			return nil, err
		}
	}

	region := zone[:len(zone)-1]
	awsCreds, err := awscredentials.NewAWSCredentials("", "", "")
	if err != nil {
		return nil, err
	}
	creds, err := awsCreds.Get()
	if err != nil {
		return nil, err
	}

	ec2 := ec2.New(
		session.New(
			&aws.Config{
				Region:      &region,
				Credentials: creds,
			},
		),
	)

	autoscaling := autoscaling.New(
		session.New(
			&aws.Config{
				Region:      &region,
				Credentials: creds,
			},
		),
	)

	return backoff.NewExponentialBackoffOps(
		&awsOps{
			Compute:      unsupported.NewUnsupportedCompute(),
			instance:     instanceID,
			instanceType: instanceType,
			ec2:          ec2,
			zone:         zone,
			region:       region,
			autoscaling:  autoscaling,
		},
		isExponentialError,
		backoff.DefaultExponentialBackoff,
	), nil

}

func (s *awsOps) filters(
	labels map[string]string,
	keys []string,
) []*ec2.Filter {
	if len(labels) == 0 {
		return nil
	}
	f := make([]*ec2.Filter, len(labels)+len(keys))
	i := 0
	for k, v := range labels {
		s := string("tag:") + k
		value := v
		f[i] = &ec2.Filter{Name: &s, Values: []*string{&value}}
		i++
	}
	for _, k := range keys {
		s := string("tag-key:") + k
		f[i] = &ec2.Filter{Name: &s}
		i++
	}
	return f
}

func (s *awsOps) tags(labels map[string]string) []*ec2.Tag {
	if len(labels) == 0 {
		return nil
	}
	t := make([]*ec2.Tag, len(labels))
	i := 0
	for k, v := range labels {
		key := k
		value := v
		t[i] = &ec2.Tag{Key: &key, Value: &value}
		i++
	}
	return t
}

func (s *awsOps) waitStatus(id string, desired string) error {
	request := &ec2.DescribeVolumesInput{VolumeIds: []*string{&id}}
	actual := ""

	_, err := task.DoRetryWithTimeout(
		func() (interface{}, bool, error) {
			awsVols, err := s.ec2.DescribeVolumes(request)
			if err != nil {
				return nil, true, err
			}

			if len(awsVols.Volumes) != 1 {
				return nil, true, fmt.Errorf("expected one volume %v got %v",
					id, len(awsVols.Volumes))
			}

			if awsVols.Volumes[0].State == nil {
				return nil, true, fmt.Errorf("Nil volume state for %v", id)
			}

			actual = *awsVols.Volumes[0].State
			if actual == desired {
				return nil, false, nil
			}

			return nil, true, fmt.Errorf(
				"Volume %v did not transition to %v current state %v",
				id, desired, actual)

		},
		cloudops.ProviderOpsTimeout,
		cloudops.ProviderOpsRetryInterval)

	return err

}

func (s *awsOps) waitAttachmentStatus(
	volumeID string,
	desired string,
	timeout time.Duration,
) (*ec2.Volume, error) {
	id := volumeID
	request := &ec2.DescribeVolumesInput{VolumeIds: []*string{&id}}
	interval := 2 * time.Second
	logrus.Infof("Waiting for state transition to %q", desired)

	f := func() (interface{}, bool, error) {
		awsVols, err := s.ec2.DescribeVolumes(request)
		if err != nil {
			return nil, false, err
		}
		if len(awsVols.Volumes) != 1 {
			return nil, false, fmt.Errorf("expected one volume %v got %v",
				volumeID, len(awsVols.Volumes))
		}

		var actual string
		vol := awsVols.Volumes[0]
		awsAttachment := vol.Attachments
		if awsAttachment == nil || len(awsAttachment) == 0 {
			// We have encountered scenarios where AWS returns a nil attachment state
			// for a volume transitioning from detaching -> attaching.
			actual = ec2.VolumeAttachmentStateDetached
		} else {
			actual = *awsAttachment[0].State
		}
		if actual == desired {
			return vol, false, nil
		}
		return nil, true, fmt.Errorf("Volume %v failed to transition to  %v current state %v",
			volumeID, desired, actual)
	}

	outVol, err := task.DoRetryWithTimeout(f, timeout, interval)
	if err != nil {
		return nil, err
	}
	if vol, ok := outVol.(*ec2.Volume); ok {
		return vol, nil
	}
	return nil, cloudops.NewStorageError(cloudops.ErrVolInval,
		fmt.Sprintf("Invalid volume object for volume %s", volumeID), "")
}

func (s *awsOps) Name() string { return "aws" }

func (s *awsOps) InstanceID() string { return s.instance }

func (s *awsOps) InspectInstance(instanceID string) (*cloudops.InstanceInfo, error) {
	inst, err := DescribeInstanceByID(s.ec2, instanceID)
	if err != nil {
		return nil, err
	}

	name := instanceID
	labels := labelsFromTags(inst.Tags)
	if nameFromTags, present := labels["Name"]; present && len(nameFromTags) > 0 {
		name = nameFromTags
	}

	instInfo := &cloudops.InstanceInfo{
		CloudResourceInfo: cloudops.CloudResourceInfo{
			Name:   name,
			ID:     *inst.InstanceId,
			Zone:   s.zone,
			Region: s.region,
			Labels: labels,
		},
	}
	return instInfo, nil
}

func (s *awsOps) InspectInstanceGroupForInstance(instanceID string) (*cloudops.InstanceGroupInfo, error) {
	selfInfo, err := s.InspectInstance(instanceID)
	if err != nil {
		return nil, err
	}

	for tag, value := range selfInfo.Labels {
		// https://docs.aws.amazon.com/autoscaling/ec2/userguide/autoscaling-tagging.html#tag-lifecycle
		if tag == "aws:autoscaling:groupName" {
			input := &autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []*string{
					aws.String(value),
				},
			}

			result, err := s.autoscaling.DescribeAutoScalingGroups(input)
			if err != nil {
				if aerr, ok := err.(awserr.Error); ok {
					return nil, aerr
				}
				return nil, err
			}

			if len(result.AutoScalingGroups) != 1 {
				return nil, fmt.Errorf("DescribeAutoScalingGroups (%v) returned %v groups, expect 1",
					value, len(result.AutoScalingGroups))
			}

			group := result.AutoScalingGroups[0]
			zones := make([]string, 0)
			for _, z := range group.AvailabilityZones {
				zones = append(zones, *z)
			}

			retval := &cloudops.InstanceGroupInfo{
				CloudResourceInfo: cloudops.CloudResourceInfo{
					Name:   *group.AutoScalingGroupName,
					Zone:   s.zone,
					Region: s.region,
					Labels: labelsFromTags(group.Tags),
				},
				Zones:              zones,
				AutoscalingEnabled: true,
				Min:                group.MinSize,
				Max:                group.MaxSize,
			}

			return retval, nil
		}
	}

	return nil, &cloudops.ErrNoInstanceGroup{}
}

func (s *awsOps) ApplyTags(volumeID string, labels map[string]string) error {
	req := &ec2.CreateTagsInput{
		Resources: []*string{&volumeID},
		Tags:      s.tags(labels),
	}
	_, err := s.ec2.CreateTags(req)
	return err
}

func (s *awsOps) RemoveTags(volumeID string, labels map[string]string) error {
	req := &ec2.DeleteTagsInput{
		Resources: []*string{&volumeID},
		Tags:      s.tags(labels),
	}
	_, err := s.ec2.DeleteTags(req)
	return err
}

func (s *awsOps) matchTag(tag *ec2.Tag, match string) bool {
	return tag.Key != nil &&
		tag.Value != nil &&
		len(*tag.Key) != 0 &&
		len(*tag.Value) != 0 &&
		*tag.Key == match
}

func (s *awsOps) DeviceMappings() (map[string]string, error) {
	instance, err := s.describe()
	if err != nil {
		return nil, err
	}

	m := make(map[string]string)
	for _, d := range instance.BlockDeviceMappings {
		if d.DeviceName != nil && d.Ebs != nil && d.Ebs.VolumeId != nil {
			devName := *d.DeviceName
			// Skip the root device
			if devName == *instance.RootDeviceName {
				continue
			}

			devicePath, err := s.getActualDevicePath(devName, *d.Ebs.VolumeId)
			if err != nil {
				return nil, cloudops.NewStorageError(
					cloudops.ErrInvalidDevicePath,
					fmt.Sprintf("unable to get actual device path for %s. %v", devName, err),
					s.instance)
			}

			m[devicePath] = *d.Ebs.VolumeId
		}
	}
	return m, nil
}

// Describe current instance.
func (s *awsOps) Describe() (interface{}, error) {
	return s.describe()
}

func (s *awsOps) describe() (*ec2.Instance, error) {
	request := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&s.instance},
	}
	out, err := s.ec2.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	if len(out.Reservations) != 1 {
		return nil, fmt.Errorf("DescribeInstances(%v) returned %v reservations, expect 1",
			s.instance, len(out.Reservations))
	}
	if len(out.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("DescribeInstances(%v) returned %v Reservations, expect 1",
			s.instance, len(out.Reservations[0].Instances))
	}
	return out.Reservations[0].Instances[0], nil
}

func (s *awsOps) getPrefixFromRootDeviceName(rootDeviceName string) (string, error) {
	devPrefix := awsDevicePrefix
	if !strings.HasPrefix(rootDeviceName, devPrefix) {
		devPrefix = awsDevicePrefixWithX
		if !strings.HasPrefix(rootDeviceName, devPrefix) {
			devPrefix = awsDevicePrefixWithH
			if !strings.HasPrefix(rootDeviceName, devPrefix) {
				return "", fmt.Errorf("unknown prefix type on root device: %s",
					rootDeviceName)
			}
		}
	}
	return devPrefix, nil
}

// getParentDevice returns the parent device of the given device path
// by following the symbolic link. It is expected that the input device
// path exists
func (s *awsOps) getParentDevice(ipDevPath string) (string, error) {
	// Check if the path is a symbolic link
	var parentDevPath string
	fi, err := os.Lstat(ipDevPath)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// input device path is a symbolic link
		// get the parent device
		output, err := filepath.EvalSymlinks(ipDevPath)
		if err != nil {
			return "", fmt.Errorf("failed to read symlink due to: %v", err)
		}
		parentDevPath = strings.TrimSpace(string(output))
	} else {
		parentDevPath = ipDevPath
	}
	return parentDevPath, nil
}

// getActualDevicePath does an os.Stat on the provided devicePath.
// If not found it will try all the different devicePrefixes provided by AWS
// such as /dev/sd and /dev/xvd and return the devicePath which is found
// or return an error
func (s *awsOps) getActualDevicePath(ipDevicePath, volumeID string) (string, error) {
	var err error
	letter := ipDevicePath[len(ipDevicePath)-1:]
	devicePath := awsDevicePrefix + letter
	if _, err = os.Stat(devicePath); err == nil {
		return s.getParentDevice(devicePath)
	}
	devicePath = awsDevicePrefixWithX + letter
	if _, err = os.Stat(devicePath); err == nil {
		return s.getParentDevice(devicePath)
	}

	devicePath = awsDevicePrefixWithH + letter
	if _, err = os.Stat(devicePath); err == nil {
		return s.getParentDevice(devicePath)
	}

	if devicePath, err = s.getNvmeDeviceFromVolumeID(volumeID); err == nil {
		if _, err = os.Stat(devicePath); err == nil {
			return devicePath, nil
		}
	}

	return "", fmt.Errorf("unable to map volume %v with block device mapping %v to an"+
		" actual device path on the host", volumeID, ipDevicePath)
}

func (s *awsOps) getNvmeDeviceFromVolumeID(volumeID string) (string, error) {
	// We will use nvme list command to find nvme device mappings
	// A typical output of nvme list looks like this
	// # nvme list
	// Node             SN                   Model                                    Namespace Usage                      Format           FW Rev
	// ---------------- -------------------- ---------------------------------------- --------- -------------------------- ---------------- --------
	// /dev/nvme0n1     vol00fd6f8c30dc619f4 Amazon Elastic Block Store               1           0.00   B / 137.44  GB    512   B +  0 B   1.0
	// /dev/nvme1n1     vol044e12c8c0af45b3d Amazon Elastic Block Store               1           0.00   B / 107.37  GB    512   B +  0 B   1.0
	trimmedVolumeID := strings.Replace(volumeID, "-", "", 1)
	out, err := sh.Command(nvmeCmd, "list").Command("grep", trimmedVolumeID).Command("awk", "{print $1}").Output()
	if err != nil {
		return "", fmt.Errorf("unable to map %v volume to an nvme device: %v", volumeID, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *awsOps) FreeDevices(
	blockDeviceMappings []interface{},
	rootDeviceName string,
) ([]string, error) {
	freeLetterTracker := []byte("fghijklmnop")
	devNamesInUse := make(map[string]string) // used as a set, values not used

	// We also need to fetch ephemeral device mappings as they are not populated
	// in blockDeviceMappings
	// See bottom of this page:
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/block-device-mapping-concepts.html?icmpid=docs_ec2_console#instance-block-device-mapping
	mappingsFromMetadata, err := metadata("block-device-mapping")
	if err != nil {
		return nil, err
	}

	devices := strings.Split(mappingsFromMetadata, "\n")
	for _, device := range devices {
		if device == "root" || device == "ami" {
			continue
		}

		devName, err := metadata("block-device-mapping/" + device)
		if err != nil {
			return nil, err
		}

		if !strings.HasPrefix(devName, "/dev/") {
			devName = "/dev/" + devName
		}

		devNamesInUse[devName] = ""
	}

	devPrefix := awsDevicePrefix
	for _, d := range blockDeviceMappings {
		dev := d.(*ec2.InstanceBlockDeviceMapping)

		if dev.DeviceName == nil {
			return nil, fmt.Errorf("Nil device name")
		}
		devName := *dev.DeviceName

		if devName == rootDeviceName {
			continue
		}

		devNamesInUse[devName] = ""
	}

	for devName := range devNamesInUse {

		// Extract the letter from the devName (e.g extract 'f' from '/dev/sdf')
		if !strings.HasPrefix(devName, devPrefix) {
			devPrefix = awsDevicePrefixWithX
			if !strings.HasPrefix(devName, devPrefix) {
				devPrefix = awsDevicePrefixWithH
				if !strings.HasPrefix(devName, devPrefix) {
					return nil, fmt.Errorf("bad device name %q", devName)
				}
			}
		}
		letter := devName[len(devPrefix):]

		// Reset devPrefix for next devices
		devPrefix = awsDevicePrefix

		// AWS instances can have the following device names
		// /dev/xvd[b-c][a-z]
		if len(letter) == 1 {
			index := letter[0] - 'f'
			if index > ('p' - 'f') {
				continue
			}

			freeLetterTracker[index] = '0' // mark as used
		} else if len(letter) == 2 {
			// We do not attach EBS volumes with "/dev/xvdc[a-z]" formats
			continue
		} else {
			return nil, fmt.Errorf("cannot parse device name %q", devName)
		}
	}

	// Set the prefix to the same one used as the root drive
	// The reason we do this is based on the virtualization type AWS might attach
	// the device "sda" at /dev/sda OR /dev/xvda. So we look at how the root device
	// is attached and use that prefix
	devPrefix, err = s.getPrefixFromRootDeviceName(rootDeviceName)
	if err != nil {
		return nil, err
	}

	free := make([]string, len(freeLetterTracker))
	count := 0
	for _, b := range freeLetterTracker {
		if b != '0' {
			free[count] = devPrefix + string(b)
			count++
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("No more free devices")
	}
	return reverse(free[:count]), nil
}

func (s *awsOps) rollbackCreate(id string, createErr error) error {
	logrus.Warnf("Rollback create volume %v, Error %v", id, createErr)
	err := s.Delete(id)
	if err != nil {
		logrus.Warnf("Rollback failed volume %v, Error %v", id, err)
	}
	return createErr
}

func (s *awsOps) refreshVol(id *string) (*ec2.Volume, error) {
	vols, err := s.Inspect([]*string{id})
	if err != nil {
		return nil, err
	}

	if len(vols) != 1 {
		return nil, fmt.Errorf("failed to get vol: %s."+
			"Found: %d volumes on inspecting", *id, len(vols))
	}

	resp, ok := vols[0].(*ec2.Volume)
	if !ok {
		return nil, cloudops.NewStorageError(cloudops.ErrVolInval,
			fmt.Sprintf("Invalid volume returned by inspect API for vol: %s", *id),
			"")
	}

	return resp, nil
}

func (s *awsOps) deleted(v *ec2.Volume) bool {
	return *v.State == ec2.VolumeStateDeleting ||
		*v.State == ec2.VolumeStateDeleted
}

func (s *awsOps) available(v *ec2.Volume) bool {
	return *v.State == ec2.VolumeStateAvailable
}

func (s *awsOps) GetDeviceID(vol interface{}) (string, error) {
	if d, ok := vol.(*ec2.Volume); ok {
		return *d.VolumeId, nil
	} else if d, ok := vol.(*ec2.Snapshot); ok {
		return *d.SnapshotId, nil
	} else {
		return "", fmt.Errorf("invalid type: %v given to GetDeviceID", vol)
	}
}

func (s *awsOps) Inspect(volumeIds []*string) ([]interface{}, error) {
	req := &ec2.DescribeVolumesInput{VolumeIds: volumeIds}
	resp, err := s.ec2.DescribeVolumes(req)
	if err != nil {
		return nil, err
	}

	var awsVols = make([]interface{}, len(resp.Volumes))
	for i, v := range resp.Volumes {
		awsVols[i] = v
	}

	return awsVols, nil
}

func (s *awsOps) Tags(volumeID string) (map[string]string, error) {
	vol, err := s.refreshVol(&volumeID)
	if err != nil {
		return nil, err
	}

	labels := make(map[string]string)
	for _, tag := range vol.Tags {
		labels[*tag.Key] = *tag.Value
	}
	return labels, nil
}

func (s *awsOps) Enumerate(
	volumeIds []*string,
	labels map[string]string,
	setIdentifier string,
) (map[string][]interface{}, error) {
	sets := make(map[string][]interface{})

	// Enumerate all volumes that have same labels.
	f := s.filters(labels, nil)
	req := &ec2.DescribeVolumesInput{Filters: f, VolumeIds: volumeIds}
	awsVols, err := s.ec2.DescribeVolumes(req)
	if err != nil {
		return nil, err
	}

	// Volume sets are identified by volumes with the same setIdentifer.
	for _, vol := range awsVols.Volumes {
		if s.deleted(vol) {
			continue
		}
		if len(setIdentifier) == 0 {
			cloudops.AddElementToMap(sets, vol, cloudops.SetIdentifierNone)
		} else {
			found := false
			for _, tag := range vol.Tags {
				if s.matchTag(tag, setIdentifier) {
					cloudops.AddElementToMap(sets, vol, *tag.Value)
					found = true
					break
				}
			}
			if !found {
				cloudops.AddElementToMap(sets, vol, cloudops.SetIdentifierNone)
			}
		}
	}

	return sets, nil
}

func (s *awsOps) Create(
	v interface{},
	labels map[string]string,
) (interface{}, error) {
	vol, ok := v.(*ec2.Volume)
	if !ok {
		return nil, cloudops.NewStorageError(cloudops.ErrVolInval,
			"Invalid volume template given", "")
	}

	req := &ec2.CreateVolumeInput{
		AvailabilityZone: vol.AvailabilityZone,
		Encrypted:        vol.Encrypted,
		KmsKeyId:         vol.KmsKeyId,
		Size:             vol.Size,
		VolumeType:       vol.VolumeType,
		SnapshotId:       vol.SnapshotId,
	}

	if len(vol.Tags) > 0 || len(labels) > 0 {
		// Need to tag volumes on creation
		tagSpec := &ec2.TagSpecification{}
		tagSpec.SetResourceType(ec2.ResourceTypeVolume)
		volTags := []*ec2.Tag{}

		for _, tag := range vol.Tags {
			// Make a copy of the keys and values
			key := *tag.Key
			value := *tag.Value
			volTags = append(volTags, &ec2.Tag{Key: &key, Value: &value})
		}

		for k, v := range labels {
			// Make a copy of the keys and values
			key := k
			value := v
			volTags = append(volTags, &ec2.Tag{Key: &key, Value: &value})
		}
		tagSpec.Tags = volTags
		req.TagSpecifications = []*ec2.TagSpecification{tagSpec}
	}

	if *vol.VolumeType == opsworks.VolumeTypeIo1 {
		req.Iops = vol.Iops
	}

	resp, err := s.ec2.CreateVolume(req)
	if err != nil {
		return nil, err
	}
	if err = s.waitStatus(
		*resp.VolumeId,
		ec2.VolumeStateAvailable,
	); err != nil {
		return nil, s.rollbackCreate(*resp.VolumeId, err)
	}
	return s.refreshVol(resp.VolumeId)
}

func (s *awsOps) DeleteFrom(id, _ string) error {
	return s.Delete(id)
}

func (s *awsOps) Delete(id string) error {
	req := &ec2.DeleteVolumeInput{VolumeId: &id}
	_, err := s.ec2.DeleteVolume(req)
	return err
}

func (s *awsOps) Attach(volumeID string) (string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	self, err := s.describe()
	if err != nil {
		return "", err
	}

	var blockDeviceMappings = make([]interface{}, len(self.BlockDeviceMappings))
	for i, b := range self.BlockDeviceMappings {
		blockDeviceMappings[i] = b
	}

	devices, err := s.FreeDevices(blockDeviceMappings, *self.RootDeviceName)
	if err != nil {
		return "", err
	}

	for _, device := range devices {
		req := &ec2.AttachVolumeInput{
			Device:     &device,
			InstanceId: &s.instance,
			VolumeId:   &volumeID,
		}
		if _, err := s.ec2.AttachVolume(req); err != nil {
			if strings.Contains(err.Error(), "is already in use") {
				logrus.Infof("Skipping device: %s as it's in use. Will try next free device", device)
				continue
			}

			return "", err
		}

		vol, err := s.waitAttachmentStatus(
			volumeID,
			ec2.VolumeAttachmentStateAttached,
			time.Minute,
		)
		if err != nil {
			return "", err
		}

		return s.DevicePath(*vol.VolumeId)
	}

	return "", fmt.Errorf("failed to attach any of the free devices. Attempted: %v", devices)
}

func (s *awsOps) Detach(volumeID string) error {
	return s.detachInternal(volumeID, s.instance)
}

func (s *awsOps) DetachFrom(volumeID, instanceName string) error {
	return s.detachInternal(volumeID, instanceName)
}

func (s *awsOps) detachInternal(volumeID, instanceName string) error {
	force := false
	req := &ec2.DetachVolumeInput{
		InstanceId: &instanceName,
		VolumeId:   &volumeID,
		Force:      &force,
	}
	if _, err := s.ec2.DetachVolume(req); err != nil {
		return err
	}
	_, err := s.waitAttachmentStatus(volumeID,
		ec2.VolumeAttachmentStateDetached,
		time.Minute,
	)
	return err
}

func (s *awsOps) Snapshot(
	volumeID string,
	readonly bool,
) (interface{}, error) {
	request := &ec2.CreateSnapshotInput{
		VolumeId: &volumeID,
	}
	return s.ec2.CreateSnapshot(request)
}

func (s *awsOps) SnapshotDelete(snapID string) error {
	request := &ec2.DeleteSnapshotInput{
		SnapshotId: &snapID,
	}

	_, err := s.ec2.DeleteSnapshot(request)
	return err
}

func (s *awsOps) DevicePath(volumeID string) (string, error) {
	vol, err := s.refreshVol(&volumeID)
	if err != nil {
		return "", err
	}

	if vol.Attachments == nil || len(vol.Attachments) == 0 {
		return "", cloudops.NewStorageError(cloudops.ErrVolDetached,
			"Volume is detached", *vol.VolumeId)
	}
	if vol.Attachments[0].InstanceId == nil {
		return "", cloudops.NewStorageError(cloudops.ErrVolInval,
			"Unable to determine volume instance attachment", "")
	}
	if s.instance != *vol.Attachments[0].InstanceId {
		return "", cloudops.NewStorageError(cloudops.ErrVolAttachedOnRemoteNode,
			fmt.Sprintf("Volume attached on %q current instance %q",
				*vol.Attachments[0].InstanceId, s.instance),
			*vol.Attachments[0].InstanceId)

	}
	if vol.Attachments[0].State == nil {
		return "", cloudops.NewStorageError(cloudops.ErrVolInval,
			"Unable to determine volume attachment state", "")
	}
	if *vol.Attachments[0].State != ec2.VolumeAttachmentStateAttached {
		return "", cloudops.NewStorageError(cloudops.ErrVolInval,
			fmt.Sprintf("Invalid state %q, volume is not attached",
				*vol.Attachments[0].State), "")
	}
	if vol.Attachments[0].Device == nil {
		return "", cloudops.NewStorageError(cloudops.ErrVolInval,
			"Unable to determine volume attachment path", "")
	}
	devicePath, err := s.getActualDevicePath(*vol.Attachments[0].Device, volumeID)
	if err != nil {
		return "", cloudops.NewStorageError(cloudops.ErrVolInval,
			err.Error(), "")
	}
	return devicePath, nil
}

func getInfoFromMetadata() (string, string, string, error) {
	zone, err := metadata("placement/availability-zone")
	if err != nil {
		return "", "", "", err
	}

	instanceID, err := metadata("instance-id")
	if err != nil {
		return "", "", "", err
	}

	instanceType, err := metadata("instance-type")
	if err != nil {
		return "", "", "", err
	}

	return zone, instanceID, instanceType, nil
}

func getInfoFromEnv() (string, string, string, error) {
	zone, err := cloudops.GetEnvValueStrict("AWS_ZONE")
	if err != nil {
		return "", "", "", err
	}

	instance, err := cloudops.GetEnvValueStrict("AWS_INSTANCE_NAME")
	if err != nil {
		return "", "", "", err
	}

	instanceType, err := cloudops.GetEnvValueStrict("AWS_INSTANCE_TYPE")
	if err != nil {
		return "", "", "", err
	}

	if _, err := credentials.NewEnvCredentials().Get(); err != nil {
		return "", "", "", ErrAWSEnvNotAvailable
	}

	return zone, instance, instanceType, nil
}

func metadata(key string) (string, error) {
	return instanceData("meta-data/" + key)
}

// instanceData retrieves instance data specified by key.
func instanceData(key string) (string, error) {
	client := http.Client{Timeout: time.Second * 3}
	url := "http://169.254.169.254/latest/" + key
	res, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		err = fmt.Errorf("Code %d returned for url %s", res.StatusCode, url)
		return "", fmt.Errorf("Error querying AWS metadata for key %s: %v", key, err)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("Error querying AWS metadata for key %s: %v", key, err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("Failed to retrieve AWS metadata for key %s: %v", key, err)
	}
	return string(body), nil
}

// DescribeInstanceByID describes the given instance by instance ID
func DescribeInstanceByID(service *ec2.EC2, id string) (*ec2.Instance, error) {
	request := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&id},
	}
	out, err := service.DescribeInstances(request)
	if err != nil {
		return nil, err
	}
	if len(out.Reservations) != 1 {
		return nil, fmt.Errorf("DescribeInstances(%v) returned %v reservations, expect 1",
			id, len(out.Reservations))
	}
	if len(out.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("DescribeInstances(%v) returned %v Reservations, expect 1",
			id, len(out.Reservations[0].Instances))
	}
	return out.Reservations[0].Instances[0], nil
}

func labelsFromTags(input interface{}) map[string]string {
	labels := make(map[string]string)
	ec2Tags, ok := input.([]*ec2.Tag)
	if ok {
		for _, tag := range ec2Tags {
			if tag == nil {
				continue
			}

			if tag.Key == nil || tag.Value == nil {
				continue
			}

			labels[*tag.Key] = *tag.Value
		}

		return labels
	}

	autoscalingTags, ok := input.([]*autoscaling.TagDescription)
	if ok {
		for _, tag := range autoscalingTags {
			if tag == nil {
				continue
			}

			if tag.Key == nil || tag.Value == nil {
				continue
			}

			labels[*tag.Key] = *tag.Value
		}

		return labels
	}

	return labels
}

func isExponentialError(err error) bool {
	// Got the list of error codes from here
	// https://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html
	awsCodes := map[string]struct{}{
		"VolumeLimitExceeded":     {},
		"AttachmentLimitExceeded": {},
		"MaxIOPSLimitExceeded":    {},
		"ResourceLimitExceeded":   {},
		"RequestLimitExceeded":    {},
		"SnapshotLimitExceeded":   {},
		"TagLimitExceeded":        {},
	}
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if _, exist := awsCodes[awsErr.Code()]; exist {
				return true
			}
		}
	}
	return false
}

func reverse(a []string) []string {
	reversed := make([]string, len(a))
	for i, item := range a {
		reversed[len(a)-i-1] = item
	}

	return reversed
}
