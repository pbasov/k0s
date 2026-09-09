//go:build !linux

// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package cplb

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/pkg/config"
)

// KubeVIP is not supported on this platform.
type KubeVIP struct {
	K0sVars        *config.CfgVars
	Config         *k0sAPI.KubeVIPSpec
	APIPort        int
	KubeConfigPath string
}

func (k *KubeVIP) Init(_ context.Context) error {
	return fmt.Errorf("%w: control plane load balancing with kube-vip on %s", errors.ErrUnsupported, runtime.GOOS)
}

func (k *KubeVIP) Start(_ context.Context) error { return nil }
func (k *KubeVIP) Stop() error                   { return nil }
