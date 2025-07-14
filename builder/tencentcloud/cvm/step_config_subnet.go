// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"
	"fmt"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/uuid"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

type stepConfigSubnet struct {
	SubnetId        string // 用户指定的子网ID
	SubnetCidrBlock string
	SubnetName      string
	Zone            string // 用户指定的子网可用区
	createdSubnet   *vpc.Subnet
}

func (s *stepConfigSubnet) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	vpcClient := state.Get("vpc_client").(*vpc.Client)
	vpcId := state.Get("vpc_id").(string)

	// 如果指定了子网ID或子网名称，则尝试使用已有子网
	if len(s.SubnetId) != 0 || len(s.SubnetName) != 0 {
		Say(state, fmt.Sprintf("Trying to use existing subnet id: %s, name: %s", s.SubnetId, s.SubnetName), "")
		req := vpc.NewDescribeSubnetsRequest()
		req.Filters = []*vpc.Filter{
			{
				Name:   common.StringPtr("vpc-id"),
				Values: common.StringPtrs([]string{vpcId}),
			},
		}
		// 搜索指定所有可用区或所有可用区中符合条件的subnet
		if s.Zone != "" {
			req.Filters = append(req.Filters,
				&vpc.Filter{
					Name:   common.StringPtr("zone"),
					Values: common.StringPtrs([]string{s.Zone}),
				})
		}
		// 空字符串作为参数会报错
		if s.SubnetId != "" {
			req.SubnetIds = []*string{&s.SubnetId}
		} else if len(s.SubnetName) != 0 {
			req.Filters = append(req.Filters,
				&vpc.Filter{
					Name:   common.StringPtr("subnet-name"),
					Values: common.StringPtrs([]string{s.SubnetName}),
				})
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
			state.Put("subnets", resp.Response.SubnetSet)
			Message(state, fmt.Sprintf("%d subnets in total.", *resp.Response.TotalCount), "Subnet found")
			return multistep.ActionContinue
		}
		return Halt(state, fmt.Errorf("the specified subnet does not exist"), "")
	}

	// 遍历候选可用区，在对应可用区内创建subnet并将subnet收集起来便于后续销毁
	// 此时subnetname一定为空，使用随机生成的名称
	s.SubnetName = fmt.Sprintf("packer_%s", uuid.TimeOrderedUUID()[:8])
	Say(state, s.SubnetName, "Trying to create a new subnet")
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

	// 创建成功后都将subnet收集起来，便于后续销毁
	s.createdSubnet = resp.Response.Subnet
	Message(state, fmt.Sprintf("subnet created: %s in zone: %s", *s.createdSubnet.SubnetId, *s.createdSubnet.Zone), "Subnet created")

	// 由于cidr冲突，不能用同一个cidr创建多个subnet，所以创建成功后直接继续
	state.Put("subnets", []*vpc.Subnet{s.createdSubnet})
	return multistep.ActionContinue
}

func (s *stepConfigSubnet) Cleanup(state multistep.StateBag) {
	// 如果没有创建subnet，则不需要删除
	if s.createdSubnet == nil {
		return
	}
	ctx := context.TODO()
	vpcClient := state.Get("vpc_client").(*vpc.Client)

	SayClean(state, "subnet")
	req := vpc.NewDeleteSubnetRequest()
	req.SubnetId = s.createdSubnet.SubnetId
	err := Retry(ctx, func(ctx context.Context) error {
		_, e := vpcClient.DeleteSubnet(req)
		return e
	})
	if err != nil {
		Error(state, err, fmt.Sprintf("Failed to delete subnet(%s), please delete it manually", *s.createdSubnet.SubnetId))
	}

}
