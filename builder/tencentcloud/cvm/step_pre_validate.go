// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cvm

import (
	"context"
	"fmt"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
)

type stepPreValidate struct {
	ForceDelete  bool
	SkipIfExists bool
}

var ImageExistsError = fmt.Errorf("Image name has exists")

func (s *stepPreValidate) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	client := state.Get("cvm_client").(*cvm.Client)

	Say(state, config.ImageName, "Trying to check image name")

	image, err := GetImageByName(ctx, client, config.ImageName)
	if err != nil {
		return Halt(state, err, "Failed to get images info")
	}

	if image != nil {
		if s.ForceDelete {
			requestID, err := DeleteImageByID(ctx, client, *image.ImageId)
			if err != nil {
				return Halt(state, err, "Failed to delete image requestID: "+requestID)
			}
		} else {
			return Halt(state, ImageExistsError, "")
		}
	}

	Message(state, "useable", "Image name")

	return multistep.ActionContinue
}

func (s *stepPreValidate) Cleanup(multistep.StateBag) {}
