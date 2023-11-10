// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

// 移除了zoneid，由subnet step生成的subnet信息提供
type stepRunInstance struct {
	InstanceType             string
	InstanceChargeType       string
	UserData                 string
	UserDataFile             string
	instanceId               string
	InstanceName             string
	DiskType                 string
	DiskSize                 int64
	HostName                 string
	InternetChargeType       string
	InternetMaxBandwidthOut  int64
	BandwidthPackageId       string
	AssociatePublicIpAddress bool
	Tags                     map[string]string
	DataDisks                []tencentCloudDataDisk
}

func (s *stepRunInstance) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("cvm_client").(*cvm.Client)

	config := state.Get("config").(*Config)
	source_image := state.Get("source_image").(*cvm.Image)
	security_group_id := state.Get("security_group_id").(string)

	password := config.Comm.SSHPassword
	if password == "" && config.Comm.WinRMPassword != "" {
		password = config.Comm.WinRMPassword
	}

	userData, err := s.getUserData(state)
	if err != nil {
		return Halt(state, err, "Failed to get user_data")
	}

	Say(state, "Trying to create a new instance", "")

	// config RunInstances parameters
	req := cvm.NewRunInstancesRequest()
	securityEnabled := !config.DisableSecurityService
	monitorEnabled := !config.DisableMonitorService
	automationEnabled := !config.DisableAutomationService
	req.EnhancedService = &cvm.EnhancedService{
		SecurityService: &cvm.RunSecurityServiceEnabled{
			Enabled: &securityEnabled,
		},
		MonitorService: &cvm.RunMonitorServiceEnabled{
			Enabled: &monitorEnabled,
		},
		AutomationService: &cvm.RunAutomationServiceEnabled{
			Enabled: &automationEnabled,
		},
	}

	instanceChargeType := s.InstanceChargeType
	if instanceChargeType == "" {
		instanceChargeType = "POSTPAID_BY_HOUR"
	}
	req.InstanceChargeType = &instanceChargeType
	req.ImageId = source_image.ImageId
	req.InstanceType = &s.InstanceType
	// TODO: Add check for system disk size, it should be larger than image system disk size.
	req.SystemDisk = &cvm.SystemDisk{
		DiskType: &s.DiskType,
		DiskSize: &s.DiskSize,
	}
	// System disk snapshot is mandatory, so if there are additional data disks,
	// length will be larger than 1.
	if source_image.SnapshotSet != nil && len(source_image.SnapshotSet) > 1 {
		Message(state, "Use source image snapshot data disks, ignore user data disk settings", "")
		var dataDisks []*cvm.DataDisk
		for _, snapshot := range source_image.SnapshotSet {
			if *snapshot.DiskUsage == "DATA_DISK" {
				var dataDisk cvm.DataDisk
				// FIXME: Currently we have no way to get original disk type
				// from data disk snapshots, and we don't allow user to overwrite
				// snapshot settings, and we cannot guarantee a certain hard-coded type
				// is not sold out, so here we use system disk type as a workaround.
				//
				// Eventually, we need to allow user to overwrite snapshot disk
				// settings.
				dataDisk.DiskType = &s.DiskType
				dataDisk.DiskSize = snapshot.DiskSize
				dataDisk.SnapshotId = snapshot.SnapshotId
				dataDisks = append(dataDisks, &dataDisk)
			}
		}
		req.DataDisks = dataDisks
	} else {
		var dataDisks []*cvm.DataDisk
		for _, disk := range s.DataDisks {
			var dataDisk cvm.DataDisk
			dataDisk.DiskType = &disk.DiskType
			dataDisk.DiskSize = &disk.DiskSize
			if disk.SnapshotId != "" {
				dataDisk.SnapshotId = &disk.SnapshotId
			}
			dataDisks = append(dataDisks, &dataDisk)
		}
		req.DataDisks = dataDisks
	}
	if s.AssociatePublicIpAddress {
		req.InternetAccessible = &cvm.InternetAccessible{
			PublicIpAssigned:        &s.AssociatePublicIpAddress,
			InternetMaxBandwidthOut: &s.InternetMaxBandwidthOut,
		}
		if s.InternetChargeType != "" {
			req.InternetAccessible.InternetChargeType = &s.InternetChargeType
		}
		if s.BandwidthPackageId != "" {
			req.InternetAccessible.BandwidthPackageId = &s.BandwidthPackageId
		}
	}
	req.InstanceName = &s.InstanceName
	loginSettings := cvm.LoginSettings{}
	if password != "" {
		loginSettings.Password = &password
	}
	if config.Comm.SSHKeyPairName != "" {
		loginSettings.KeyIds = []*string{&config.Comm.SSHKeyPairName}
	}
	req.LoginSettings = &loginSettings
	req.SecurityGroupIds = []*string{&security_group_id}
	req.ClientToken = &s.InstanceName
	req.HostName = &s.HostName
	req.UserData = &userData
	var tags []*cvm.Tag
	for k, v := range s.Tags {
		k := k
		v := v
		tags = append(tags, &cvm.Tag{
			Key:   &k,
			Value: &v,
		})
	}
	resourceType := "instance"
	if len(tags) > 0 {
		req.TagSpecification = []*cvm.TagSpecification{
			{
				ResourceType: &resourceType,
				Tags:         tags,
			},
		}
	}
	// 遍历subnet列表，依次尝试建立instance
	subnets, ok := state.GetOk("subnets")
	if !ok {
		Halt(state, fmt.Errorf("no subnets in state"), "Cannot get subnets info when starting instance")
	}
	// 腾讯云开机时返回instanceid后还需要等待实例状态为running才可认为开机成功。
	for _, subnet := range subnets.([]*vpc.Subnet) {
		var instanceId []string
		instanceId, err = s.CreateCvmInstance(ctx, state, subnet, req)
		if err == nil {
			s.instanceId = instanceId[0]
			break
		}
		// 尝试删除已有的instanceId，避免资源泄露
		for _, id := range instanceId {
			if id != "" {
				req := cvm.NewTerminateInstancesRequest()
				req.InstanceIds = []*string{&id}
				terminateErr := Retry(ctx, func(ctx context.Context) error {
					_, e := client.TerminateInstances(req)
					return e
				})
				// 如果删除失败，且不是因为instanceId不存在，则报错
				// instanceId不存在代表之前开机不成功，此处不需要再次删除。若是LAUNCH_FAILED会预到Code=InvalidInstanceId.NotFound，跳过尝试下一个subnet继续尝试开机即可
				if terminateErr != nil && terminateErr.(*errors.TencentCloudSDKError).Code != "InvalidInstanceId.NotFound" {
					// undefined behavior, just halt
					Halt(state, terminateErr, fmt.Sprintf("Failed to terminate instance(%s), may need to delete it manually", id))
				}
			}
		}
	}
	// 最后一次开机也不成功，报错
	if err != nil {
		return Halt(state, fmt.Errorf("tried %d configurations but no luck", len(subnets.([]*vpc.Subnet))), "Failed to run instance")
	}

	describeReq := cvm.NewDescribeInstancesRequest()
	describeReq.InstanceIds = []*string{&s.instanceId}
	var describeResp *cvm.DescribeInstancesResponse
	err = Retry(ctx, func(ctx context.Context) error {
		var e error
		describeResp, e = client.DescribeInstancesWithContext(ctx, describeReq)
		return e
	})
	if err != nil {
		return Halt(state, err, "Failed to wait for instance ready")
	}

	state.Put("instance", describeResp.Response.InstanceSet[0])
	// instance_id is the generic term used so that users can have access to the
	// instance id inside of the provisioners, used in step_provision.
	state.Put("instance_id", s.instanceId)
	Message(state, s.instanceId, "Instance created")

	return multistep.ActionContinue
}

func (s *stepRunInstance) getUserData(_ multistep.StateBag) (string, error) {
	userData := s.UserData

	if userData == "" && s.UserDataFile != "" {
		data, err := os.ReadFile(s.UserDataFile)
		if err != nil {
			return "", err
		}
		userData = string(data)
	}

	userData = base64.StdEncoding.EncodeToString([]byte(userData))
	log.Printf(fmt.Sprintf("[DEBUG]getUserData: user_data: %s", userData))

	return userData, nil
}

func (s *stepRunInstance) Cleanup(state multistep.StateBag) {
	if s.instanceId == "" {
		return
	}
	ctx := context.TODO()
	client := state.Get("cvm_client").(*cvm.Client)

	SayClean(state, "instance")

	req := cvm.NewTerminateInstancesRequest()
	req.InstanceIds = []*string{&s.instanceId}
	err := Retry(ctx, func(ctx context.Context) error {
		_, e := client.TerminateInstances(req)
		return e
	})
	if err != nil {
		Error(state, err, fmt.Sprintf("Failed to terminate instance(%s), please delete it manually", s.instanceId))
	}
}

func (s *stepRunInstance) CreateCvmInstance(ctx context.Context, state multistep.StateBag, subnet *vpc.Subnet, req *cvm.RunInstancesRequest) ([]string, error) {
	client := state.Get("cvm_client").(*cvm.Client)
	vpcId := state.Get("vpc_id").(string)
	Say(state,
		fmt.Sprintf("instance-type: %s, subnet-id: %s, zone: %s",
			s.InstanceType, *subnet.SubnetId, *subnet.Zone,
		), "Try to create instance")
	req.VirtualPrivateCloud = &cvm.VirtualPrivateCloud{
		VpcId:    &vpcId,
		SubnetId: subnet.SubnetId,
	}
	req.Placement = &cvm.Placement{
		Zone: subnet.Zone,
	}
	var resp *cvm.RunInstancesResponse
	err := Retry(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.RunInstances(req)
		return e
	})
	if err != nil {
		// halt会返回终止信号并且记录error，存在error在state中会导致最终执行标记为失败
		// 此处只需要记录日志，因此使用say
		Say(state, fmt.Sprintf("%s", err), "Failed to run instance")
		return make([]string, 0), err
	}

	if len(resp.Response.InstanceIdSet) != 1 {
		return make([]string, 0), fmt.Errorf("expect 1 instance id, got %d", len(resp.Response.InstanceIdSet))
	}

	instanceId := *resp.Response.InstanceIdSet[0]
	Message(state, "Waiting for instance ready", "")

	// 如果资源不足或者配置有错误如ip冲突会造成状态为LAUNCH_FAILED。
	err = WaitForInstance(ctx, client, s.instanceId, "RUNNING", 1800)
	if err != nil {
		return []string{instanceId}, fmt.Errorf("failed to wait for instance ready, %w", err)
	}
	return []string{instanceId}, nil
}
