//go:build linux

// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package cplb

import (
	"testing"

	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kube-vip is configured entirely through its environment, so this is the whole
// contract between k0s and it. The BGP peer string in particular is positional
// and silently misread if a field is dropped, which no runtime error would
// catch.
func newKubeVIP(t *testing.T, spec *k0sAPI.KubeVIPSpec) *KubeVIP {
	t.Helper()
	require.Empty(t, spec.Validate())
	return &KubeVIP{
		K0sVars:        &config.CfgVars{CertRootDir: "/var/lib/k0s/pki"},
		Config:         spec,
		APIPort:        6443,
		KubeConfigPath: "/var/lib/k0s/pki/admin.conf",
	}
}

func bgpSpec(peers ...k0sAPI.KubeVIPBGPPeer) *k0sAPI.KubeVIPSpec {
	return &k0sAPI.KubeVIPSpec{
		Mode:       k0sAPI.KubeVIPModeBGP,
		VirtualIPs: []string{"10.44.100.10/32"},
		BGP: &k0sAPI.KubeVIPBGPSpec{
			LocalAS:         65001,
			RouterID:        "10.44.0.11",
			SourceInterface: "eth0",
			Peers:           peers,
		},
	}
}

func TestKubeVIP_buildEnv_BGP(t *testing.T) {
	k := newKubeVIP(t, bgpSpec(k0sAPI.KubeVIPBGPPeer{Address: "10.44.0.1", AS: 65000, Port: 179}))

	env, err := k.buildEnv()
	require.NoError(t, err)

	assert.Contains(t, env, "address=10.44.100.10")
	assert.Contains(t, env, "vip_subnet=32")
	// dummyvip0, so that iface.FirstPublicAddress never sees the VIP.
	assert.Contains(t, env, "vip_interface="+k0sAPI.CPLBDummyInterface)
	assert.Contains(t, env, "port=6443")
	assert.Contains(t, env, "cp_enable=true")
	assert.Contains(t, env, "bgp_enable=true")
	assert.Contains(t, env, "bgp_as=65001")
	assert.Contains(t, env, "bgp_routerid=10.44.0.11")
	assert.Contains(t, env, "bgp_sourceif=eth0")

	// BGP announces from every node, so electing one announcer would throw the
	// multipath away and put the VIP back on a lease the API server has to serve.
	assert.Contains(t, env, "vip_leaderelection=false")
	assert.NotContains(t, env, "vip_arp=true")

	// The check is what replaces leader election, and it needs the cluster CA
	// or it fails TLS and announces nothing at all.
	assert.Contains(t, env, "control_plane_health_check_address=https://127.0.0.1:6443/readyz")
	assert.Contains(t, env, "control_plane_health_check_ca_path=/var/lib/k0s/pki/ca.crt")
}

func TestKubeVIP_buildEnv_peerString(t *testing.T) {
	// <address>:<AS>:<password>:<multihop>:<port>:<mpbgp>:<BFD>
	tests := []struct {
		name string
		peer k0sAPI.KubeVIPBGPPeer
		want string
	}{
		{
			name: "plain peer",
			peer: k0sAPI.KubeVIPBGPPeer{Address: "10.44.0.1", AS: 65000, Port: 179},
			want: "bgp_peers=10.44.0.1:65000::false:179::false;300;300;3",
		},
		{
			name: "with BFD",
			peer: k0sAPI.KubeVIPBGPPeer{
				Address: "10.44.0.1", AS: 65000, Port: 179,
				BFD: &k0sAPI.KubeVIPBFDSpec{
					ReceiveIntervalMillis: 300, TransmitIntervalMillis: 300, DetectMultiplier: 3,
				},
			},
			want: "bgp_peers=10.44.0.1:65000::false:179::true;300;300;3",
		},
		{
			name: "password, multihop and a non-default port",
			peer: k0sAPI.KubeVIPBGPPeer{
				Address: "10.44.0.1", AS: 65000, Password: "s3cret", MultiHop: true, Port: 1179,
			},
			want: "bgp_peers=10.44.0.1:65000:s3cret:true:1179::false;300;300;3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			k := newKubeVIP(t, bgpSpec(test.peer))
			env, err := k.buildEnv()
			require.NoError(t, err)
			assert.Contains(t, env, test.want)
		})
	}
}

func TestKubeVIP_buildEnv_multiplePeers(t *testing.T) {
	k := newKubeVIP(t, bgpSpec(
		k0sAPI.KubeVIPBGPPeer{Address: "10.44.0.1", AS: 65000, Port: 179},
		k0sAPI.KubeVIPBGPPeer{Address: "10.44.0.2", AS: 65000, Port: 179},
	))

	env, err := k.buildEnv()
	require.NoError(t, err)
	assert.Contains(t, env,
		"bgp_peers=10.44.0.1:65000::false:179::false;300;300;3,10.44.0.2:65000::false:179::false;300;300;3")
}

func TestKubeVIP_buildEnv_ARP(t *testing.T) {
	k := newKubeVIP(t, &k0sAPI.KubeVIPSpec{
		Mode:       k0sAPI.KubeVIPModeARP,
		Interface:  "eth0",
		VirtualIPs: []string{"10.44.0.100/32"},
	})

	env, err := k.buildEnv()
	require.NoError(t, err)

	// Two nodes answering ARP for one address is a broken network, so the
	// election is not optional here.
	assert.Contains(t, env, "vip_arp=true")
	assert.Contains(t, env, "vip_leaderelection=true")
	assert.Contains(t, env, "vip_interface=eth0")
	assert.NotContains(t, env, "bgp_enable=true")

	// ARP has the lease to fail over with, so the health check is off by
	// default and must not appear.
	for _, e := range env {
		assert.NotContains(t, e, "control_plane_health_check_address")
	}
}

func TestKubeVIP_buildEnv_healthCheckTuning(t *testing.T) {
	spec := bgpSpec(k0sAPI.KubeVIPBGPPeer{Address: "10.44.0.1", AS: 65000, Port: 179})
	spec.HealthCheck = &k0sAPI.KubeVIPHealthCheckSpec{
		PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 2,
	}
	k := newKubeVIP(t, spec)

	env, err := k.buildEnv()
	require.NoError(t, err)
	assert.Contains(t, env, "control_plane_health_check_period_seconds=2")
	assert.Contains(t, env, "control_plane_health_check_timeout_seconds=1")
	assert.Contains(t, env, "control_plane_health_check_failure_threshold=2")
}
