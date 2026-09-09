// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package v1beta1

import (
	"errors"
	"fmt"
	"net"
)

// KubeVIPSpec configures kube-vip as the control plane load balancer.
//
// The reason this is a CPLB type rather than a workload is start order. A VIP
// for the API server has to exist before the API server does, and anything
// scheduled by Kubernetes cannot. k0s supervises the CPLB process itself, from
// node config, so kube-vip starts in the same phase keepalived does.
type KubeVIPSpec struct {
	// VirtualIPs is the list of virtual IP addresses managed by kube-vip.
	// Each entry is a CIDR, e.g. 192.168.0.100/32.
	//
	// Exactly one is accepted today. kube-vip takes a single address per
	// manager instance, so a second entry could only be honoured by running a
	// second instance, and silently serving just the first would leave the
	// others in the API server certificate but unreachable. The field is a
	// list to match KeepalivedSpec and to leave room to lift this.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +listType=set
	VirtualIPs []string `json:"virtualIPs"`

	// Mode selects how the virtual IP is made reachable.
	// ARP answers ARP requests for the VIP on Interface, and requires leader
	// election so that exactly one node answers.
	// BGP advertises the VIP from every node and lets the upstream router
	// balance across them, and needs no leader election.
	// +kubebuilder:default=ARP
	// +kubebuilder:validation:Enum=ARP;BGP
	Mode KubeVIPMode `json:"mode,omitempty"`

	// Interface is the NIC that carries the VIP. If not specified, k0s uses
	// the interface that owns the default route in ARP mode, and the CPLB
	// dummy interface in BGP mode, where the address only has to exist
	// locally. See the note on dummyvip0 in the component for why that
	// default matters.
	Interface string `json:"interface,omitempty"`

	// BGP holds the BGP configuration. Required when Mode is BGP.
	BGP *KubeVIPBGPSpec `json:"bgp,omitempty"`

	// HealthCheck controls whether a node withdraws its own advertisement when
	// its local API server stops being healthy. This is what replaces leader
	// election in BGP mode.
	HealthCheck *KubeVIPHealthCheckSpec `json:"healthCheck,omitempty"`
}

// CPLBDummyInterface is the interface k0s uses to hold control plane virtual
// IPs. iface.FirstPublicAddress skips it, so addresses parked here are never
// mistaken for the node's own address.
const CPLBDummyInterface = "dummyvip0"

// KubeVIPMode selects the mechanism used to make the VIP reachable.
type KubeVIPMode string

const (
	// KubeVIPModeARP answers ARP for the VIP from the elected leader.
	KubeVIPModeARP KubeVIPMode = "ARP"
	// KubeVIPModeBGP advertises the VIP over BGP from every node.
	KubeVIPModeBGP KubeVIPMode = "BGP"
)

// KubeVIPBGPSpec configures the BGP speaker.
type KubeVIPBGPSpec struct {
	// LocalAS is this node's autonomous system number.
	// +kubebuilder:validation:Minimum=1
	LocalAS uint32 `json:"localAS"`

	// RouterID is the BGP router ID. If not specified, k0s derives it from the
	// address of SourceInterface, which is what makes a single cluster-wide
	// config usable on every node.
	RouterID string `json:"routerID,omitempty"`

	// SourceInterface is the interface whose address is used as the BGP source
	// and, unless RouterID is set, as the router ID. Defaults to the interface
	// owning the default route.
	SourceInterface string `json:"sourceInterface,omitempty"`

	// Peers is the list of BGP neighbors.
	// +kubebuilder:validation:MinItems=1
	Peers []KubeVIPBGPPeer `json:"peers"`
}

// KubeVIPBGPPeer is a single BGP neighbor.
type KubeVIPBGPPeer struct {
	// Address of the peer.
	Address string `json:"address"`

	// AS is the peer's autonomous system number.
	// +kubebuilder:validation:Minimum=1
	AS uint32 `json:"as"`

	// Password enables MD5 authentication with the peer.
	Password string `json:"password,omitempty"`

	// MultiHop allows the peer to be more than one hop away.
	MultiHop bool `json:"multiHop,omitempty"`

	// Port of the peer. Defaults to 179.
	// +kubebuilder:default=179
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port uint16 `json:"port,omitempty"`

	// BFD enables Bidirectional Forwarding Detection for this peer, so that a
	// dead speaker is noticed in well under a BGP hold time.
	BFD *KubeVIPBFDSpec `json:"bfd,omitempty"`
}

// KubeVIPBFDSpec configures BFD for a peer.
type KubeVIPBFDSpec struct {
	// ReceiveIntervalMillis is the minimum receive interval. Defaults to 300.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	ReceiveIntervalMillis uint32 `json:"receiveIntervalMillis,omitempty"`
	// TransmitIntervalMillis is the desired transmit interval. Defaults to 300.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	TransmitIntervalMillis uint32 `json:"transmitIntervalMillis,omitempty"`
	// DetectMultiplier is the detection multiplier. Defaults to 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	DetectMultiplier uint32 `json:"detectMultiplier,omitempty"`
}

// KubeVIPHealthCheckSpec configures the local API server health check.
type KubeVIPHealthCheckSpec struct {
	// Enabled turns the health check on. Defaults to true in BGP mode, where
	// it is the only failure detection there is, and false in ARP mode, where
	// leader election covers it.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// PeriodSeconds between checks. Defaults to 5.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	PeriodSeconds uint32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds for each check. Defaults to 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds uint32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is the number of consecutive failures before the
	// advertisement is withdrawn. Defaults to 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	FailureThreshold uint32 `json:"failureThreshold,omitempty"`
}

// IsEnabled reports whether the health check should run for the given mode.
func (h *KubeVIPHealthCheckSpec) IsEnabled(mode KubeVIPMode) bool {
	if h != nil && h.Enabled != nil {
		return *h.Enabled
	}
	return mode == KubeVIPModeBGP
}

// Validate validates the KubeVIPSpec and fills in defaults.
func (k *KubeVIPSpec) Validate() (errs []error) {
	if k == nil {
		return nil
	}

	switch k.Mode {
	case KubeVIPModeARP, KubeVIPModeBGP:
	case "":
		k.Mode = KubeVIPModeARP
	default:
		errs = append(errs, fmt.Errorf("unsupported kube-vip mode: %q, allowed values: %s, %s",
			k.Mode, KubeVIPModeARP, KubeVIPModeBGP))
	}

	switch len(k.VirtualIPs) {
	case 1:
	case 0:
		errs = append(errs, errors.New("at least one virtual IP must be defined"))
	default:
		errs = append(errs, fmt.Errorf("kube-vip accepts a single virtual IP, got %d", len(k.VirtualIPs)))
	}
	for _, vip := range k.VirtualIPs {
		if _, _, err := net.ParseCIDR(vip); err != nil {
			errs = append(errs, fmt.Errorf("invalid virtual IP %q: expected a CIDR: %w", vip, err))
		}
	}

	if k.Interface == "" {
		if k.Mode == KubeVIPModeBGP {
			// In BGP mode the address only has to exist locally; reachability
			// comes from the routing table, not from the link.
			//
			// dummyvip0 rather than lo, and this is not cosmetic. k0s detects
			// its own node address with iface.FirstPublicAddress, which walks
			// interfaces in kernel order and returns the first address outside
			// 127.0.0.0/8. lo is index 1, so a VIP placed there is picked up as
			// the node's own address, and every etcd member then advertises the
			// same address and loses quorum for good. That function already
			// skips dummyvip0, because it is the interface CPLB uses. Putting
			// the VIP there makes the failure impossible instead of merely
			// documented.
			k.Interface = CPLBDummyInterface
		} else {
			nic, err := getDefaultNIC()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to detect the default network interface: %w", err))
			}
			k.Interface = nic
		}
	}

	switch k.Mode {
	case KubeVIPModeBGP:
		if k.BGP == nil {
			errs = append(errs, errors.New("bgp must be defined when mode is BGP"))
		} else {
			errs = append(errs, k.BGP.validate()...)
		}
	case KubeVIPModeARP:
		if k.BGP != nil {
			errs = append(errs, errors.New("bgp must not be defined when mode is ARP"))
		}
	}

	if k.HealthCheck != nil {
		if k.HealthCheck.PeriodSeconds == 0 {
			k.HealthCheck.PeriodSeconds = 5
		}
		if k.HealthCheck.TimeoutSeconds == 0 {
			k.HealthCheck.TimeoutSeconds = 3
		}
		if k.HealthCheck.FailureThreshold == 0 {
			k.HealthCheck.FailureThreshold = 3
		}
	}

	return errs
}

func (b *KubeVIPBGPSpec) validate() (errs []error) {
	if b.LocalAS == 0 {
		errs = append(errs, errors.New("bgp.localAS must be set"))
	}
	if b.RouterID != "" && net.ParseIP(b.RouterID) == nil {
		errs = append(errs, fmt.Errorf("invalid bgp.routerID %q: expected an IP address", b.RouterID))
	}
	if b.SourceInterface == "" {
		nic, err := getDefaultNIC()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to detect the default network interface: %w", err))
		}
		b.SourceInterface = nic
	}
	if len(b.Peers) == 0 {
		errs = append(errs, errors.New("at least one bgp peer must be defined"))
	}
	for i := range b.Peers {
		p := &b.Peers[i]
		if net.ParseIP(p.Address) == nil {
			errs = append(errs, fmt.Errorf("invalid bgp peer address %q", p.Address))
		}
		if p.AS == 0 {
			errs = append(errs, fmt.Errorf("bgp peer %s: as must be set", p.Address))
		}
		if p.Port == 0 {
			p.Port = 179
		}
		if p.BFD != nil {
			if p.BFD.ReceiveIntervalMillis == 0 {
				p.BFD.ReceiveIntervalMillis = 300
			}
			if p.BFD.TransmitIntervalMillis == 0 {
				p.BFD.TransmitIntervalMillis = 300
			}
			if p.BFD.DetectMultiplier == 0 {
				p.BFD.DetectMultiplier = 3
			}
		}
	}
	return errs
}
