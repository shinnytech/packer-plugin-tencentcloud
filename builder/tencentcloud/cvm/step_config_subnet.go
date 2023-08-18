// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"
	"fmt"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

type stepConfigSubnet struct {
	SubnetId        string
	SubnetCidrBlock string
	SubnetName      string
	Zone            string
	isCreate        bool
}

func (s *stepConfigSubnet) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	vpcClient := state.Get("vpc_client").(*vpc.Client)
	cvmClient := state.Get("cvm_client").(*cvm.Client)

	vpcId := state.Get("vpc_id").(string)
	instanceType := state.Get("instance_type").(string)
	// 根据机型自动选择可用区
	if s.Zone == "" {
		req := cvm.NewDescribeZoneInstanceConfigInfosRequest()
		req.Filters = []*cvm.Filter{
			{
				Name:   common.StringPtr("instance-type"),
				Values: common.StringPtrs([]string{instanceType}),
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
			s.Zone = *resp.Response.InstanceTypeQuotaSet[0].Zone
			state.Put("zone", s.Zone)
		} else {
			Say(state, fmt.Sprintf("The instance type %s isn't available in this region."+
				"\n You can change to other regions.", instanceType), "")
			state.Put("error", fmt.Errorf("The instance type %s isn't available in this region."+
				"\n You can change to other regions.", instanceType))
			return multistep.ActionHalt
		}
	}

	// 如果指定了子网ID或子网名称，则尝试使用已有子网
	if len(s.SubnetId) != 0 || len(s.SubnetName) != 0 {
		Say(state, s.SubnetId, "Trying to use existing subnet")
		req := vpc.NewDescribeSubnetsRequest()
		// 空字符串作为参数会报错
		if len(s.SubnetId) != 0 {
			req.SubnetIds = []*string{&s.SubnetId}
		}
		if len(s.SubnetName) != 0 {
			req.Filters = []*vpc.Filter{
				{
					Name:   common.StringPtr("subnet-name"),
					Values: common.StringPtrs([]string{s.SubnetName}),
				},
				{
					Name:   common.StringPtr("zone"),
					Values: common.StringPtrs([]string{s.Zone}),
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
			if *resp.Response.SubnetSet[0].VpcId != vpcId {
				return Halt(state, fmt.Errorf("The specified subnet(%s) does not belong to the specified vpc(%s)",
					s.SubnetId, vpcId), "")
			}
			state.Put("subnet_id", *resp.Response.SubnetSet[0].SubnetId)
			Message(state, *resp.Response.SubnetSet[0].SubnetName, "Subnet found")
			return multistep.ActionContinue
		}
		return Halt(state, fmt.Errorf("The specified subnet(%s) does not exist", s.SubnetId), "")
	}

	Say(state, "Trying to create a new subnet", "")

	req := vpc.NewCreateSubnetRequest()
	req.VpcId = &vpcId
	req.SubnetName = &s.SubnetName
	req.CidrBlock = &s.SubnetCidrBlock
	req.Zone = &s.Zone
	var resp *vpc.CreateSubnetResponse
	err := Retry(ctx, func(ctx context.Context) error {
		var e error
		resp, e = vpcClient.CreateSubnet(req)
		return e
	})
	if err != nil {
		return Halt(state, err, "Failed to create subnet")
	}

	s.isCreate = true
	s.SubnetId = *resp.Response.Subnet.SubnetId
	state.Put("subnet_id", s.SubnetId)
	Message(state, s.SubnetId, "Subnet created")

	return multistep.ActionContinue
}

func (s *stepConfigSubnet) Cleanup(state multistep.StateBag) {
	if !s.isCreate {
		return
	}

	ctx := context.TODO()
	vpcClient := state.Get("vpc_client").(*vpc.Client)

	SayClean(state, "subnet")

	req := vpc.NewDeleteSubnetRequest()
	req.SubnetId = &s.SubnetId
	err := Retry(ctx, func(ctx context.Context) error {
		_, e := vpcClient.DeleteSubnet(req)
		return e
	})
	if err != nil {
		Error(state, err, fmt.Sprintf("Failed to delete subnet(%s), please delete it manually", s.SubnetId))
	}
}
