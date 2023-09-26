// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/uuid"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

type stepConfigSubnet struct {
	SubnetIds       []string // 可能会新建多个subnet
	SubnetCidrBlock string
	SubnetName      string
	Zones           []string // 多个可用区内可能有相同的subnetName的subnet
	isCreate        bool
}

func (s *stepConfigSubnet) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	vpcClient := state.Get("vpc_client").(*vpc.Client)
	cvmClient := state.Get("cvm_client").(*cvm.Client)
	vpcId := state.Get("vpc_id").(string)
	instanceType := state.Get("config").(*Config).InstanceType

	// 根据机型自动选择可用区
	if s.Zones[0] == "" {
		Say(state, fmt.Sprintf("Try to get available zones for instance: %s", instanceType), "")
		req := cvm.NewDescribeZoneInstanceConfigInfosRequest()
		req.Filters = []*cvm.Filter{
			{
				Name:   common.StringPtr("instance-type"),
				Values: common.StringPtrs([]string{instanceType}),
			},
			{
				Name:   common.StringPtr("instance-charge-type"),
				Values: common.StringPtrs([]string{"POSTPAID_BY_HOUR"}),
			},
		}
		var resp *cvm.DescribeZoneInstanceConfigInfosResponse
		err := Retry(ctx, func(ctx context.Context) error {
			var e error
			resp, e = cvmClient.DescribeZoneInstanceConfigInfos(req)
			return e
		})
		if err != nil {
			return Halt(state, err, "Failed to get available zones instance config")
		}
		if len(resp.Response.InstanceTypeQuotaSet) > 0 {
			zones := make([]string, 0)
			Say(state, fmt.Sprintf("length:%d", len(resp.Response.InstanceTypeQuotaSet)), "")
			for _, z := range resp.Response.InstanceTypeQuotaSet {
				zones = append(zones, *z.Zone)
			}
			s.Zones = zones
			Say(state, fmt.Sprintf("Found zones: %s", strings.Join(s.Zones, ",")), "")
		} else {
			Say(state, fmt.Sprintf("The instance type %s isn't available in this region."+
				"\n You can change to other regions.", instanceType), "")
			state.Put("error", fmt.Errorf("The instance type %s isn't available in this region."+
				"\n You can change to other regions.", instanceType))
			return multistep.ActionHalt
		}
	}

	// 如果指定了子网ID或子网名称，则尝试使用已有子网
	if s.SubnetIds[0] != "" || len(s.SubnetName) != 0 {
		Say(state, fmt.Sprintf("Trying to use existing subnet id: %s, name: %s", s.SubnetIds[0], s.SubnetName), "")
		req := vpc.NewDescribeSubnetsRequest()
		// 空字符串作为参数会报错
		if s.SubnetIds[0] != "" {
			req.SubnetIds = []*string{&s.SubnetIds[0]}
		}
		if len(s.SubnetName) != 0 {
			// s.zones列表长度不能超过5,取最后五个
			if len(s.Zones) > 5 {
				s.Zones = s.Zones[len(s.Zones)-5:]
			}
			// 搜索机型在售所有可用区内符合subnet名称的subnet
			req.Filters = []*vpc.Filter{
				{
					Name:   common.StringPtr("subnet-name"),
					Values: common.StringPtrs([]string{s.SubnetName}),
				},
				{
					Name:   common.StringPtr("zone"),
					Values: common.StringPtrs(s.Zones),
				},
			}
		}
		var resp *vpc.DescribeSubnetsResponse
		err := Retry(ctx, func(ctx context.Context) error {
			var e error
			resp, e = vpcClient.DescribeSubnets(req)
			return e
		})
		if err != nil {
			return Halt(state, err, "Failed to get subnet info")
		}
		if *resp.Response.TotalCount > 0 {
			s.isCreate = false
			for _, subnet := range resp.Response.SubnetSet {
				if *subnet.VpcId != vpcId {
					return Halt(state, fmt.Errorf("the specified subnet(%s) does not belong to the specified vpc(%s)",
						s.SubnetIds, vpcId), "")
				}
			}
			state.Put("subnets", resp.Response.SubnetSet)
			Message(state, fmt.Sprintf("%d subnets in total.", *resp.Response.TotalCount), "Subnet found")
			return multistep.ActionContinue
		}
		return Halt(state, fmt.Errorf("The specified subnet(%s) does not exist", s.SubnetIds), "")
	}

	// 遍历候选可用区，在对应可用区内创建subnet并将subnet收集起来便于后续销毁
	s.isCreate = true
	if s.SubnetName == "" {
		s.SubnetName = fmt.Sprintf("packer_%s", uuid.TimeOrderedUUID()[:8])
	}
	Say(state, s.SubnetName, "Trying to create a new subnet")
	subnets := make([]*vpc.Subnet, 0)
	defer func() {
		subnetIds := make([]string, 0)
		for _, subnet := range subnets {
			subnetIds = append(subnetIds, *subnet.SubnetId)
		}
		s.SubnetIds = subnetIds
	}()
	for _, zone := range s.Zones {
		req := vpc.NewCreateSubnetRequest()
		req.VpcId = &vpcId
		req.SubnetName = &s.SubnetName
		req.CidrBlock = &s.SubnetCidrBlock
		req.Zone = &zone
		var resp *vpc.CreateSubnetResponse
		err := Retry(ctx, func(ctx context.Context) error {
			var e error
			resp, e = vpcClient.CreateSubnet(req)
			return e
		})
		if err != nil {
			return Halt(state, err, "Failed to create subnet")
		}

		subnets = append(subnets, resp.Response.Subnet)
		Message(state, fmt.Sprintf("%d subnets in total.", len(s.SubnetIds)), "Subnet created")
	}
	state.Put("subnets", subnets)

	return multistep.ActionContinue
}

func (s *stepConfigSubnet) Cleanup(state multistep.StateBag) {
	if !s.isCreate {
		return
	}

	ctx := context.TODO()
	vpcClient := state.Get("vpc_client").(*vpc.Client)

	SayClean(state, "subnet")

	for _, id := range s.SubnetIds {
		req := vpc.NewDeleteSubnetRequest()
		req.SubnetId = &id
		err := Retry(ctx, func(ctx context.Context) error {
			_, e := vpcClient.DeleteSubnet(req)
			return e
		})
		if err != nil {
			Error(state, err, fmt.Sprintf("Failed to delete subnet(%s), please delete it manually", s.SubnetIds))
		}
	}
}
