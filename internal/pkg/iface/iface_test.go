// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"errors"
	"iter"
	"net"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	up   = net.FlagUp
	down = net.Flags(0)
	loUp = net.FlagUp | net.FlagLoopback
)

type iface struct {
	name  string
	flags net.Flags
	addrs []string
}

// fakeHost turns a list of interfaces into the two arguments of
// firstPublicAddress. Interfaces named in failing report an error instead of
// their addresses, the way a link that disappears mid-walk does.
func fakeHost(ifaces []iface, failing ...string) ([]net.Interface, func(net.Interface) (iter.Seq[*net.IPNet], error)) {
	netIfaces := make([]net.Interface, 0, len(ifaces))
	byName := make(map[string][]string, len(ifaces))
	for i, f := range ifaces {
		netIfaces = append(netIfaces, net.Interface{Index: i + 1, Name: f.name, Flags: f.flags})
		byName[f.name] = f.addrs
	}

	return netIfaces, func(i net.Interface) (iter.Seq[*net.IPNet], error) {
		if slices.Contains(failing, i.Name) {
			return nil, errors.New("failed to list IP addresses")
		}
		cidrs := byName[i.Name]
		return func(yield func(*net.IPNet) bool) {
			for _, cidr := range cidrs {
				ip, ipnet, err := net.ParseCIDR(cidr)
				if err != nil {
					panic(err)
				}
				ipnet.IP = ip
				if !yield(ipnet) {
					return
				}
			}
		}, nil
	}
}

func TestFirstPublicAddress(t *testing.T) {
	// Interfaces are listed in kernel order, so lo comes first in every case
	// that has one, which is what makes these regressions possible at all.
	tests := []struct {
		name   string
		ifaces []iface
		want   string
	}{
		{
			name: "plain host",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			// The kube-vip BGP failure: the VIP is on lo on every node, and
			// picking it makes every etcd member advertise the same address.
			name: "virtual IP on the loopback interface is not the node address",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8", "10.44.100.10/32"}},
				{"eth0", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			// Every p04 host carries the same 169.254.3.1 on a BMC USB NIC.
			name: "link-local address on an earlier interface is skipped",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"enxbe3af2b6059f", up, []string{"169.254.3.1/16"}},
				{"eth0", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			name: "interfaces that are down are skipped",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", down, []string{"10.44.0.11/24"}},
				{"eth1", up, []string{"10.44.0.12/24"}},
			},
			want: "10.44.0.12",
		},
		{
			name: "the CPLB dummy interface is skipped",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"dummyvip0", up, []string{"10.44.100.10/32"}},
				{"eth0", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			name: "CNI interfaces are skipped",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"vxlan.calico", up, []string{"10.244.13.0/32"}},
				{"kube-bridge", up, []string{"10.244.0.1/24"}},
				{"cali1234", up, []string{"10.244.0.5/32"}},
				{"veth1234", up, []string{"10.244.0.6/32"}},
				{"eth0", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			name: "IPv4 wins over an IPv6 address seen earlier",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", up, []string{"2001:db8::11/64"}},
				{"eth1", up, []string{"10.44.0.11/24"}},
			},
			want: "10.44.0.11",
		},
		{
			name: "IPv6 is used when no interface has IPv4",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8", "::1/128"}},
				{"eth0", up, []string{"fe80::1/64", "2001:db8::11/64"}},
			},
			want: "2001:db8::11",
		},
		{
			name: "a link-local IPv6 address is not an IPv6 fallback",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", up, []string{"fe80::1/64"}},
			},
			want: "127.0.0.1",
		},
		{
			name: "unique local IPv6 addresses are usable",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", up, []string{"fd00::11/64"}},
			},
			want: "fd00::11",
		},
		{
			name: "no usable address at all",
			ifaces: []iface{
				{"lo", loUp, []string{"127.0.0.1/8"}},
				{"eth0", up, []string{"169.254.3.1/16"}},
			},
			want: "127.0.0.1",
		},
		{
			name:   "no interfaces at all",
			ifaces: nil,
			want:   "127.0.0.1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ifaces, addressesOf := fakeHost(test.ifaces)
			got, err := firstPublicAddress(ifaces, addressesOf)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFirstPublicAddress_interfaceErrorsAreSkipped(t *testing.T) {
	ifaces, addressesOf := fakeHost([]iface{
		{"lo", loUp, []string{"127.0.0.1/8"}},
		{"eth0", up, []string{"10.44.0.11/24"}},
		{"eth1", up, []string{"10.44.0.12/24"}},
	}, "eth0")

	got, err := firstPublicAddress(ifaces, addressesOf)
	require.NoError(t, err)
	assert.Equal(t, "10.44.0.12", got)
}

// FirstPublicAddress talks to the real host, so all that can be asserted is
// that it agrees with the rules and never returns something unusable.
func TestFirstPublicAddress_onThisHost(t *testing.T) {
	got, err := FirstPublicAddress()
	require.NoError(t, err)

	ip := net.ParseIP(got)
	require.NotNil(t, ip, "not an IP address: %q", got)
	if got != "127.0.0.1" {
		assert.True(t, ip.IsGlobalUnicast(), "%s is not a global unicast address", got)
	}
}
