// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package v1beta1

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKubeVIPSpec_Validate(t *testing.T) {
	bgp := func() *KubeVIPBGPSpec {
		return &KubeVIPBGPSpec{
			LocalAS:         65001,
			SourceInterface: "eth0",
			Peers:           []KubeVIPBGPPeer{{Address: "10.0.0.1", AS: 65000}},
		}
	}

	tests := []struct {
		name    string
		spec    *KubeVIPSpec
		wantErr string
	}{
		{
			name: "BGP, minimal",
			spec: &KubeVIPSpec{Mode: KubeVIPModeBGP, VirtualIPs: []string{"10.0.0.10/32"}, BGP: bgp()},
		},
		{
			name:    "no virtual IPs",
			spec:    &KubeVIPSpec{Mode: KubeVIPModeBGP, BGP: bgp()},
			wantErr: "at least one virtual IP must be defined",
		},
		{
			// kube-vip serves one address per manager, so extras must be
			// rejected rather than silently dropped.
			name: "more than one virtual IP",
			spec: &KubeVIPSpec{
				Mode:       KubeVIPModeBGP,
				VirtualIPs: []string{"10.0.0.10/32", "10.0.0.11/32"},
				BGP:        bgp(),
			},
			wantErr: "kube-vip accepts a single virtual IP, got 2",
		},
		{
			name:    "virtual IP is not a CIDR",
			spec:    &KubeVIPSpec{Mode: KubeVIPModeBGP, VirtualIPs: []string{"10.0.0.10"}, BGP: bgp()},
			wantErr: `invalid virtual IP "10.0.0.10"`,
		},
		{
			name:    "unsupported mode",
			spec:    &KubeVIPSpec{Mode: "VRRP", VirtualIPs: []string{"10.0.0.10/32"}},
			wantErr: `unsupported kube-vip mode: "VRRP"`,
		},
		{
			name:    "BGP mode without a bgp section",
			spec:    &KubeVIPSpec{Mode: KubeVIPModeBGP, VirtualIPs: []string{"10.0.0.10/32"}},
			wantErr: "bgp must be defined when mode is BGP",
		},
		{
			name: "ARP mode with a bgp section",
			spec: &KubeVIPSpec{
				Mode: KubeVIPModeARP, Interface: "eth0",
				VirtualIPs: []string{"10.0.0.10/32"}, BGP: bgp(),
			},
			wantErr: "bgp must not be defined when mode is ARP",
		},
		{
			name: "BGP peer without an AS",
			spec: &KubeVIPSpec{
				Mode: KubeVIPModeBGP, VirtualIPs: []string{"10.0.0.10/32"},
				BGP: &KubeVIPBGPSpec{
					LocalAS: 65001, SourceInterface: "eth0",
					Peers: []KubeVIPBGPPeer{{Address: "10.0.0.1"}},
				},
			},
			wantErr: "bgp peer 10.0.0.1: as must be set",
		},
		{
			name: "BGP peer address is not an IP",
			spec: &KubeVIPSpec{
				Mode: KubeVIPModeBGP, VirtualIPs: []string{"10.0.0.10/32"},
				BGP: &KubeVIPBGPSpec{
					LocalAS: 65001, SourceInterface: "eth0",
					Peers: []KubeVIPBGPPeer{{Address: "not-an-ip", AS: 65000}},
				},
			},
			wantErr: `invalid bgp peer address "not-an-ip"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errs := test.spec.Validate()
			if test.wantErr == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs, "expected an error mentioning %q", test.wantErr)
			assert.ErrorContains(t, errors.Join(errs...), test.wantErr)
		})
	}
}

func TestKubeVIPSpec_Defaults(t *testing.T) {
	t.Run("BGP parks the VIP on the CPLB dummy interface", func(t *testing.T) {
		// Not cosmetic: iface.FirstPublicAddress skips dummyvip0 by name, so a
		// VIP placed there can never be mistaken for the node's own address.
		spec := &KubeVIPSpec{
			Mode:       KubeVIPModeBGP,
			VirtualIPs: []string{"10.0.0.10/32"},
			BGP: &KubeVIPBGPSpec{
				LocalAS: 65001, SourceInterface: "eth0",
				Peers: []KubeVIPBGPPeer{{Address: "10.0.0.1", AS: 65000}},
			},
		}
		require.Empty(t, spec.Validate())
		assert.Equal(t, CPLBDummyInterface, spec.Interface)
	})

	t.Run("peer and BFD defaults are filled in", func(t *testing.T) {
		spec := &KubeVIPSpec{
			Mode:       KubeVIPModeBGP,
			VirtualIPs: []string{"10.0.0.10/32"},
			BGP: &KubeVIPBGPSpec{
				LocalAS: 65001, SourceInterface: "eth0",
				Peers: []KubeVIPBGPPeer{{Address: "10.0.0.1", AS: 65000, BFD: &KubeVIPBFDSpec{}}},
			},
		}
		require.Empty(t, spec.Validate())
		peer := spec.BGP.Peers[0]
		assert.Equal(t, uint16(179), peer.Port)
		assert.Equal(t, uint32(300), peer.BFD.ReceiveIntervalMillis)
		assert.Equal(t, uint32(300), peer.BFD.TransmitIntervalMillis)
		assert.Equal(t, uint32(3), peer.BFD.DetectMultiplier)
	})

	t.Run("mode defaults to ARP", func(t *testing.T) {
		spec := &KubeVIPSpec{VirtualIPs: []string{"10.0.0.10/32"}, Interface: "eth0"}
		_ = spec.Validate()
		assert.Equal(t, KubeVIPModeARP, spec.Mode)
	})
}

func TestKubeVIPHealthCheck_IsEnabled(t *testing.T) {
	enabled, disabled := true, false

	// BGP has no leader election, so withdrawal on an unhealthy local API
	// server is the only failure detection there is; ARP has the lease.
	assert.True(t, (*KubeVIPHealthCheckSpec)(nil).IsEnabled(KubeVIPModeBGP))
	assert.False(t, (*KubeVIPHealthCheckSpec)(nil).IsEnabled(KubeVIPModeARP))
	assert.False(t, (&KubeVIPHealthCheckSpec{Enabled: &disabled}).IsEnabled(KubeVIPModeBGP))
	assert.True(t, (&KubeVIPHealthCheckSpec{Enabled: &enabled}).IsEnabled(KubeVIPModeARP))
}

func TestControlPlaneLoadBalancingSpec_VirtualIPs(t *testing.T) {
	tests := []struct {
		name string
		spec *ControlPlaneLoadBalancingSpec
		want []string
	}{
		{name: "nil", spec: nil},
		{
			name: "disabled contributes nothing",
			spec: &ControlPlaneLoadBalancingSpec{
				Enabled: false, Type: CPLBTypeKubeVIP,
				KubeVIP: &KubeVIPSpec{VirtualIPs: []string{"10.0.0.10/32"}},
			},
		},
		{
			name: "kube-vip",
			spec: &ControlPlaneLoadBalancingSpec{
				Enabled: true, Type: CPLBTypeKubeVIP,
				KubeVIP: &KubeVIPSpec{VirtualIPs: []string{"10.0.0.10/32"}},
			},
			want: []string{"10.0.0.10/32"},
		},
		{
			name: "keepalived, across instances",
			spec: &ControlPlaneLoadBalancingSpec{
				Enabled: true, Type: CPLBTypeKeepalived,
				Keepalived: &KeepalivedSpec{VRRPInstances: []VRRPInstance{
					{VirtualIPs: []string{"10.0.0.10/32"}},
					{VirtualIPs: []string{"10.0.0.11/32"}},
				}},
			},
			want: []string{"10.0.0.10/32", "10.0.0.11/32"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, test.spec.VirtualIPs())
		})
	}
}
