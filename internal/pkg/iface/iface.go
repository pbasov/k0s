// SPDX-FileCopyrightText: 2021 k0s authors
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"fmt"
	"iter"
	"net"
	"strings"

	"github.com/sirupsen/logrus"
)

// FirstPublicAddress returns the first globally routable IPv4 address found on
// a host interface, or, if no interface carries one, the first globally
// routable IPv6 address. It is how k0s determines its own node address when
// spec.api.address is not configured.
//
// Interfaces are walked in kernel order, which makes the choice sensitive to
// what else is present on the host, so several classes of address are excluded
// because they cannot be a node's own address:
//
//   - anything on the loopback interface. A virtual IP parked on lo by
//     kube-vip or keepalived is reachable locally but belongs to the cluster,
//     not to the node. lo is interface index 1, so without this it would win
//     over every real NIC.
//   - anything on an interface that is down.
//   - addresses that are not global unicast: loopback, link-local (169.254/16
//     and fe80::/10, as used by BMC and other management NICs), multicast and
//     the unspecified address.
//   - addresses on well-known CNI interfaces, by name.
//
// Note that the pod network is only excluded by those interface names, not by
// checking the configured pod CIDR.
func FirstPublicAddress() (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1", fmt.Errorf("failed to list network interfaces: %w", err)
	}

	return firstPublicAddress(ifs, interfaceAddrs)
}

// firstPublicAddress is the body of [FirstPublicAddress], with the host lookups
// injected so that the selection rules can be tested.
func firstPublicAddress(ifs []net.Interface, addressesOf func(net.Interface) (iter.Seq[*net.IPNet], error)) (string, error) {
	ipv6addr := ""
	for _, i := range ifs {
		switch {
		// Skip the loopback interface. Every address on it is either a
		// loopback address or something parked there by another component,
		// such as a kube-vip or keepalived VIP. Neither is this node's own
		// address, and lo is walked first.
		case i.Flags&net.FlagLoopback != 0:
			continue
		// Skip interfaces that are down
		case i.Flags&net.FlagUp == 0:
			continue
		// Skip calico CNI interface
		case i.Name == "vxlan.calico":
			continue
		// Skip kube-router CNI interface
		case i.Name == "kube-bridge":
			continue
		// Skip k0s CPLB interface
		case i.Name == "dummyvip0":
			continue
		// Skip kube-router pod CNI interfaces
		case strings.HasPrefix(i.Name, "veth"):
			continue
		// Skip calico pod CNI interfaces
		case strings.HasPrefix(i.Name, "cali"):
			continue
		}

		addresses, err := addressesOf(i)
		if err != nil {
			logrus.WithError(err).Warn("Skipping network interface ", i.Name)
			continue
		}
		for a := range addresses {
			// Only a globally routable unicast address can be this node's own
			// address. This rejects loopback, link-local, multicast and the
			// unspecified address, while keeping private ranges such as
			// 10.0.0.0/8 and fc00::/7.
			if !a.IP.IsGlobalUnicast() {
				continue
			}
			if a.IP.To4() != nil {
				return a.IP.String(), nil
			}
			if ipv6addr == "" {
				ipv6addr = a.IP.String()
			}
		}
	}
	if ipv6addr != "" {
		return ipv6addr, nil
	}

	logrus.Warn("failed to find any non-local, non podnetwork addresses on host, defaulting public address to 127.0.0.1")
	return "127.0.0.1", nil
}
