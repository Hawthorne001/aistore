// +build !gcp

// Package cloud contains implementation of various cloud providers.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cloud

import (
	"github.com/NVIDIA/aistore/cluster"
)

type (
	gcpProvider struct {
		emptyCloudProvider
		t cluster.Target
	}
)

func NewGCP(t cluster.Target) (cluster.CloudProvider, error) {
	return &gcpProvider{emptyCloudProvider{}, t}, nil
}
