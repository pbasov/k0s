// SPDX-FileCopyrightText: 2026 k0s authors
// SPDX-License-Identifier: Apache-2.0

package cplb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"github.com/k0sproject/k0s/pkg/assets"
	"github.com/k0sproject/k0s/pkg/config"
	"github.com/k0sproject/k0s/pkg/supervisor"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// KubeVIP runs kube-vip as the control plane load balancer.
//
// It is supervised as a plain process, exactly like keepalived, and for the
// same reason: the VIP has to be up before kube-apiserver is, so it cannot be
// anything the API server has to schedule. Running it here also means k0s knows
// the virtual IPs from its own node configuration, which is what lets it keep
// them out of its own address detection.
type KubeVIP struct {
	K0sVars        *config.CfgVars
	Config         *k0sAPI.KubeVIPSpec
	APIPort        int
	KubeConfigPath string

	supervisor     *supervisor.Supervisor
	executablePath string
	configFilePath string
	log            *logrus.Entry
}

// Init stages the kube-vip executable and prepares its configuration path.
func (k *KubeVIP) Init(_ context.Context) error {
	if k.Config == nil {
		return nil
	}
	k.log = logrus.WithField("component", "CPLB")

	k.configFilePath = filepath.Join(k.K0sVars.RunDir, "kube-vip.env")

	var err error
	k.executablePath, err = assets.StageExecutable(k.K0sVars.BinDir, "kube-vip")
	if err != nil {
		return fmt.Errorf("failed to stage the kube-vip executable: %w", err)
	}
	return nil
}

// Start builds the kube-vip environment and supervises the process.
func (k *KubeVIP) Start(ctx context.Context) error {
	if k.Config == nil || len(k.Config.VirtualIPs) == 0 {
		k.log.Warn("No virtual IPs defined, skipping kube-vip start")
		return nil
	}

	// kube-vip adds the address itself, but the link has to exist first, and
	// on this interface specifically: k0s's own address detection skips
	// dummyvip0, which is what stops the VIP being adopted as the node
	// address and destroying etcd quorum.
	if k.Config.Interface == k0sAPI.CPLBDummyInterface {
		if err := ensureDummyLink(k.Config.Interface); err != nil {
			return fmt.Errorf("failed to ensure the %s interface: %w", k.Config.Interface, err)
		}
	}

	env, err := k.buildEnv()
	if err != nil {
		return err
	}

	// Written for operators to read. kube-vip is configured through its
	// environment, which is invisible on a running node, so the same settings
	// are dumped to a file next to the other CPLB state.
	if err := os.WriteFile(k.configFilePath, []byte(strings.Join(env, "\n")+"\n"), 0600); err != nil {
		k.log.WithError(err).Warn("Failed to write the kube-vip environment dump")
	}

	k.log.Infof("Starting kube-vip in %s mode", k.Config.Mode)
	k.supervisor = &supervisor.Supervisor{
		Name:    "kube-vip",
		BinPath: k.executablePath,
		Args:    []string{"manager"},
		Env:     env,
		RunDir:  k.K0sVars.RunDir,
		DataDir: k.K0sVars.DataDir,
	}

	return k.supervisor.Supervise(ctx)
}

// Stop stops kube-vip and removes the virtual IPs it added.
//
// The addresses are removed explicitly rather than left behind: a VIP still
// present on a stopped node keeps attracting traffic in ARP mode, and in BGP
// mode it would be advertised again the moment the process restarts, before it
// has checked whether the local API server is healthy.
func (k *KubeVIP) Stop() error {
	if k.supervisor != nil {
		k.log.Info("Stopping kube-vip")
		if err := k.supervisor.Stop(); err != nil {
			return fmt.Errorf("failed to stop kube-vip: %w", err)
		}
	}
	return k.removeVirtualIPs()
}

// buildEnv renders the kube-vip configuration as environment variables.
//
// kube-vip is configured through its environment rather than through its
// config file because that is the interface its own manifests use, and the
// one its documentation describes. Several settings, BFD among them, are only
// reachable this way.
func (k *KubeVIP) buildEnv() ([]string, error) {
	vip, err := firstVirtualIP(k.Config.VirtualIPs)
	if err != nil {
		return nil, err
	}
	ones, _ := vip.Mask.Size()

	env := []string{
		"address=" + vip.IP.String(),
		// vip_subnet, not vip_cidr: the latter was renamed in kube-vip v0.8.0
		// and is silently ignored by current versions.
		"vip_subnet=" + strconv.Itoa(ones),
		"vip_interface=" + k.Config.Interface,
		"port=" + strconv.Itoa(k.APIPort),
		"cp_enable=true",
		"cp_namespace=kube-system",
		"svc_enable=false",
		// kube-vip needs a kubeconfig file to exist at startup, but builds its
		// client lazily and never contacts the API server on the BGP path.
		"k8s_config_file=" + k.KubeConfigPath,
	}

	switch k.Config.Mode {
	case k0sAPI.KubeVIPModeARP:
		// ARP needs exactly one node answering for the address, so leader
		// election is not optional here.
		env = append(env, "vip_arp=true", "vip_leaderelection=true")
	case k0sAPI.KubeVIPModeBGP:
		// Every node advertises and the upstream router balances across them.
		// Electing a single advertiser would discard the multipath and put the
		// VIP back on a lease that needs a writable API server.
		env = append(env, "bgp_enable=true", "vip_leaderelection=false")
		bgpEnv, err := k.bgpEnv()
		if err != nil {
			return nil, err
		}
		env = append(env, bgpEnv...)
	}

	if k.Config.HealthCheck.IsEnabled(k.Config.Mode) {
		period, timeout, threshold := uint32(5), uint32(3), uint32(3)
		if hc := k.Config.HealthCheck; hc != nil {
			period, timeout, threshold = hc.PeriodSeconds, hc.TimeoutSeconds, hc.FailureThreshold
		}
		// The check is an unauthenticated GET against the local API server. It
		// needs the cluster CA to verify the certificate, and it needs the API
		// server to permit anonymous access to this one path, or it fails with
		// HTTP 401 and kube-vip advertises nothing at all.
		env = append(env,
			fmt.Sprintf("control_plane_health_check_address=https://127.0.0.1:%d/readyz", k.APIPort),
			"control_plane_health_check_ca_path="+filepath.Join(k.K0sVars.CertRootDir, "ca.crt"),
			fmt.Sprintf("control_plane_health_check_period_seconds=%d", period),
			fmt.Sprintf("control_plane_health_check_timeout_seconds=%d", timeout),
			fmt.Sprintf("control_plane_health_check_failure_threshold=%d", threshold),
		)
	}

	return env, nil
}

func (k *KubeVIP) bgpEnv() ([]string, error) {
	bgp := k.Config.BGP

	routerID := bgp.RouterID
	if routerID == "" {
		// Deriving the router ID from the source interface is what allows one
		// cluster-wide configuration to be correct on every node.
		addr, err := firstIPv4OfInterface(bgp.SourceInterface)
		if err != nil {
			return nil, fmt.Errorf("failed to derive the BGP router ID from interface %q: %w", bgp.SourceInterface, err)
		}
		routerID = addr
	}

	// bgp_peers rather than bgp_peeraddress/bgp_peeras: it is the only form
	// that carries per-peer BFD settings, and setting both would define each
	// neighbor twice. Layout is
	//   <address>:<AS>:<password>:<multihop>:<port>:<mpbgp>:<BFD>
	// where the BFD field is enable;receive;transmit;multiplier.
	peers := make([]string, 0, len(bgp.Peers))
	for _, p := range bgp.Peers {
		bfd := "false;300;300;3"
		if p.BFD != nil {
			bfd = fmt.Sprintf("true;%d;%d;%d", p.BFD.ReceiveIntervalMillis, p.BFD.TransmitIntervalMillis, p.BFD.DetectMultiplier)
		}
		peers = append(peers, fmt.Sprintf("%s:%d:%s:%t:%d::%s",
			p.Address, p.AS, p.Password, p.MultiHop, p.Port, bfd))
	}

	return []string{
		"bgp_as=" + strconv.FormatUint(uint64(bgp.LocalAS), 10),
		"bgp_routerid=" + routerID,
		"bgp_sourceif=" + bgp.SourceInterface,
		"bgp_peers=" + strings.Join(peers, ","),
	}, nil
}

// removeVirtualIPs deletes the configured VIPs from the interface.
func (k *KubeVIP) removeVirtualIPs() error {
	if k.Config == nil || k.Config.Interface == "" {
		return nil
	}
	link, err := netlink.LinkByName(k.Config.Interface)
	if err != nil {
		return fmt.Errorf("failed to find interface %q: %w", k.Config.Interface, err)
	}
	for _, vip := range k.Config.VirtualIPs {
		addr, err := netlink.ParseAddr(vip)
		if err != nil {
			return fmt.Errorf("failed to parse virtual IP %q: %w", vip, err)
		}
		if err := netlink.AddrDel(link, addr); err != nil && !os.IsNotExist(err) {
			k.log.WithError(err).Warnf("Failed to remove virtual IP %s from %s", vip, k.Config.Interface)
		}
	}
	return nil
}

// ensureDummyLink creates the dummy interface if it is missing and brings it up.
func ensureDummyLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if !errors.As(err, &netlink.LinkNotFoundError{}) {
			return err
		}
		link = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		if link, err = netlink.LinkByName(name); err != nil {
			return err
		}
	}
	return netlink.LinkSetUp(link)
}

func firstVirtualIP(vips []string) (*net.IPNet, error) {
	if len(vips) == 0 {
		return nil, fmt.Errorf("no virtual IPs configured")
	}
	ip, ipnet, err := net.ParseCIDR(vips[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse virtual IP %q: %w", vips[0], err)
	}
	ipnet.IP = ip
	return ipnet, nil
}

func firstIPv4OfInterface(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if a.IP != nil && !a.IP.IsLoopback() {
			return a.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address on interface %s", name)
}
