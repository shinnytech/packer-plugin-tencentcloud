// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
)

type stepDetachTempKeyPair struct {
}

func (s *stepDetachTempKeyPair) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	client := state.Get("cvm_client").(*cvm.Client)

	if _, ok := state.GetOk("temporary_key_pair_id"); !ok {
		return multistep.ActionContinue
	}

	keyId := state.Get("temporary_key_pair_id").(string)
	instance := state.Get("instance").(*cvm.Instance)

	// 生成镜像前先关机，避免创建镜像过程中发生重启导致服务意外启动
	Say(state, *instance.InstanceName, "Trying to stop instance")

	stopReq := cvm.NewStopInstancesRequest()
	stopReq.InstanceIds = []*string{instance.InstanceId}
	err := Retry(ctx, func(ctx context.Context) error {
		_, e := client.StopInstances(stopReq)
		return e
	})
	if err != nil {
		return Halt(state, err, "Failed to stop instance")
	}
	Message(state, "Waiting for instance stop", "")
	err = WaitForInstance(ctx, client, *instance.InstanceId, "STOPPED", 1800)
	if err != nil {
		return Halt(state, err, "Failed to wait for instance to be stopped")
	}

	Say(state, keyId, "Trying to detach keypair")

	req := cvm.NewDisassociateInstancesKeyPairsRequest()
	req.KeyIds = []*string{&keyId}
	req.InstanceIds = []*string{instance.InstanceId}
	req.ForceStop = common.BoolPtr(false)
	err = Retry(ctx, func(ctx context.Context) error {
		_, e := client.DisassociateInstancesKeyPairs(req)
		return e
	})
	if err != nil {
		return Halt(state, err, "Fail to detach keypair from instance")
	}

	Message(state, "Waiting for keypair detached", "")
	err = WaitForInstance(ctx, client, *instance.InstanceId, "STOPPED", 1800)
	if err != nil {
		return Halt(state, err, "Failed to wait for keypair detached")
	}

	Message(state, "Keypair detached", "")

	return multistep.ActionContinue
}

func (s *stepDetachTempKeyPair) Cleanup(state multistep.StateBag) {}
