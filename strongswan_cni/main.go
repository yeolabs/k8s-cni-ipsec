// Copyright 2014 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"syscall"

	"io/ioutil"

	"os"
	"os/exec"
	"strings"

	"bytes"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils"
	"github.com/vishvananda/netlink"
)

const defaultBrName = "cni0"

type NetConf struct {
	types.NetConf
	BrName       string `json:"bridge"`
	IsGW         bool   `json:"isGateway"`
	IsDefaultGW  bool   `json:"isDefaultGateway"`
	ForceAddress bool   `json:"forceAddress"`
	IPMasq       bool   `json:"ipMasq"`
	MTU          int    `json:"mtu"`
	HairpinMode  bool   `json:"hairpinMode"`
	PromiscMode  bool   `json:"promiscMode"`
}

type gwInfo struct {
	gws               []net.IPNet
	family            int
	defaultRouteFound bool
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadNetConf(bytes []byte) (*NetConf, string, error) {
	n := &NetConf{
		BrName: defaultBrName,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}
	return n, n.CNIVersion, nil
}

// calcGateways processes the results from the IPAM plugin and does the
// following for each IP family:
//    - Calculates and compiles a list of gateway addresses
//    - Adds a default route if needed
func calcGateways(result *current.Result, n *NetConf) (*gwInfo, *gwInfo, error) {

	gwsV4 := &gwInfo{}
	gwsV6 := &gwInfo{}

	for _, ipc := range result.IPs {

		// Determine if this config is IPv4 or IPv6
		var gws *gwInfo
		defaultNet := &net.IPNet{}
		switch {
		case ipc.Address.IP.To4() != nil:
			gws = gwsV4
			gws.family = netlink.FAMILY_V4
			defaultNet.IP = net.IPv4zero
		case len(ipc.Address.IP) == net.IPv6len:
			gws = gwsV6
			gws.family = netlink.FAMILY_V6
			defaultNet.IP = net.IPv6zero
		default:
			return nil, nil, fmt.Errorf("Unknown IP object: %v", ipc)
		}
		defaultNet.Mask = net.IPMask(defaultNet.IP)

		// All IPs currently refer to the container interface
		ipc.Interface = current.Int(2)

		// If not provided, calculate the gateway address corresponding
		// to the selected IP address
		if ipc.Gateway == nil && n.IsGW {
			ipc.Gateway = calcGatewayIP(&ipc.Address)
		}

		// Add a default route for this family using the current
		// gateway address if necessary.
		if n.IsDefaultGW && !gws.defaultRouteFound {
			for _, route := range result.Routes {
				if route.GW != nil && defaultNet.String() == route.Dst.String() {
					gws.defaultRouteFound = true
					break
				}
			}
			if !gws.defaultRouteFound {
				result.Routes = append(
					result.Routes,
					&types.Route{Dst: *defaultNet, GW: ipc.Gateway},
				)
				gws.defaultRouteFound = true
			}
		}

		// Append this gateway address to the list of gateways
		if n.IsGW {
			gw := net.IPNet{
				IP:   ipc.Gateway,
				Mask: ipc.Address.Mask,
			}
			gws.gws = append(gws.gws, gw)
		}
	}
	return gwsV4, gwsV6, nil
}

func ensureBridgeAddr(br *netlink.Bridge, family int, ipn *net.IPNet, forceAddress bool) error {
	addrs, err := netlink.AddrList(br, family)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not get list of IP addresses: %v", err)
	}

	ipnStr := ipn.String()
	for _, a := range addrs {

		// string comp is actually easiest for doing IPNet comps
		if a.IPNet.String() == ipnStr {
			return nil
		}

		// Multiple IPv6 addresses are allowed on the bridge if the
		// corresponding subnets do not overlap. For IPv4 or for
		// overlapping IPv6 subnets, reconfigure the IP address if
		// forceAddress is true, otherwise throw an error.
		if family == netlink.FAMILY_V4 || a.IPNet.Contains(ipn.IP) || ipn.Contains(a.IPNet.IP) {
			if forceAddress {
				if err = deleteBridgeAddr(br, a.IPNet); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("%q already has an IP address different from %v", br.Name, ipnStr)
			}
		}
	}

	addr := &netlink.Addr{IPNet: ipn, Label: ""}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return fmt.Errorf("could not add IP address to %q: %v", br.Name, err)
	}
	return nil
}

func deleteBridgeAddr(br *netlink.Bridge, ipn *net.IPNet) error {
	addr := &netlink.Addr{IPNet: ipn, Label: ""}

	if err := netlink.AddrDel(br, addr); err != nil {
		return fmt.Errorf("could not remove IP address from %q: %v", br.Name, err)
	}

	return nil
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func ensureBridge(brName string, mtu int, promiscMode bool) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}

	err := netlink.LinkAdd(br)
	if err != nil && err != syscall.EEXIST {
		return nil, fmt.Errorf("could not add %q: %v", brName, err)
	}

	if promiscMode {
		if err := netlink.SetPromiscOn(br); err != nil {
			return nil, fmt.Errorf("could not set promiscuous mode on %q: %v", brName, err)
		}
	}

	// Re-fetch link to read all attributes and if it already existed,
	// ensure it's really a bridge with similar configuration
	br, err = bridgeByName(brName)
	if err != nil {
		return nil, err
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	return br, nil
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int, hairpinMode bool) (*current.Interface, *current.Interface, error) {
	contIface := &current.Interface{}
	hostIface := &current.Interface{}

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}
		contIface.Name = containerVeth.Name
		contIface.Mac = containerVeth.HardwareAddr.String()
		contIface.Sandbox = netns.Path()
		hostIface.Name = hostVeth.Name
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// need to lookup hostVeth again as its index has changed during ns move
	hostVeth, err := netlink.LinkByName(hostIface.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to lookup %q: %v", hostIface.Name, err)
	}
	hostIface.Mac = hostVeth.Attrs().HardwareAddr.String()

	// connect host veth end to the bridge
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		return nil, nil, fmt.Errorf("failed to connect %q to bridge %v: %v", hostVeth.Attrs().Name, br.Attrs().Name, err)
	}

	// set hairpin mode
	if err = netlink.LinkSetHairpin(hostVeth, hairpinMode); err != nil {
		return nil, nil, fmt.Errorf("failed to setup hairpin mode for %v: %v", hostVeth.Attrs().Name, err)
	}

	return hostIface, contIface, nil
}

func calcGatewayIP(ipn *net.IPNet) net.IP {
	nid := ipn.IP.Mask(ipn.Mask)
	return ip.NextIP(nid)
}

func setupBridge(n *NetConf) (*netlink.Bridge, *current.Interface, error) {
	// create bridge if necessary
	br, err := ensureBridge(n.BrName, n.MTU, n.PromiscMode)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create bridge %q: %v", n.BrName, err)
	}

	return br, &current.Interface{
		Name: br.Attrs().Name,
		Mac:  br.Attrs().HardwareAddr.String(),
	}, nil
}

// disableIPV6DAD disables IPv6 Duplicate Address Detection (DAD)
// for an interface.
func disableIPV6DAD(ifName string) error {
	f := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_dad", ifName)
	return ioutil.WriteFile(f, []byte("0"), 0644)
}

func enableIPForward(family int) error {
	if family == netlink.FAMILY_V4 {
		return ip.EnableIP4Forward()
	}
	return ip.EnableIP6Forward()
}

func cmdAdd(args *skel.CmdArgs) error {
	n, cniVersion, err := loadNetConf(args.StdinData)

	log.Println("strongswan", n, cniVersion)
	if err != nil {
		return err
	}

	if n.IsDefaultGW {
		n.IsGW = true
	}

	if n.HairpinMode && n.PromiscMode {
		return fmt.Errorf("cannot set hairpin mode and promiscous mode at the same time.")
	}

	br, brInterface, err := setupBridge(n)
	log.Println("strongswan", br, brInterface)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	log.Println("strongswan", netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	hostInterface, containerInterface, err := setupVeth(netns, br, args.IfName, n.MTU, n.HairpinMode)
	if err != nil {
		return err
	}

	// run the IPAM plugin and get back the config to apply
	r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// Convert whatever the IPAM result was into the current Result type
	result, err := current.NewResultFromResult(r)
	if err != nil {
		return err
	}

	if len(result.IPs) == 0 {
		return errors.New("IPAM plugin returned missing IP config")
	}

	result.Interfaces = []*current.Interface{brInterface, hostInterface, containerInterface}

	// Gather gateway information for each IP family
	gwsV4, gwsV6, err := calcGateways(result, n)
	if err != nil {
		return err
	}

	// Configure the container hardware address and IP address(es)
	if err := netns.Do(func(_ ns.NetNS) error {
		// Disable IPv6 DAD just in case hairpin mode is enabled on the
		// bridge. Hairpin mode causes echos of neighbor solicitation
		// packets, which causes DAD failures.
		// TODO: (short term) Disable DAD conditional on actual hairpin mode
		// TODO: (long term) Use enhanced DAD when that becomes available in kernels.
		if err := disableIPV6DAD(args.IfName); err != nil {
			return err
		}

		if err := ipam.ConfigureIface(args.IfName, result); err != nil {
			return err
		}

		if result.IPs[0].Address.IP.To4() != nil {
			if err := ip.SetHWAddrByIP(args.IfName, result.IPs[0].Address.IP, nil /* TODO IPv6 */); err != nil {
				return err
			}
		}

		// Refetch the veth since its MAC address may changed
		link, err := netlink.LinkByName(args.IfName)
		if err != nil {
			return fmt.Errorf("could not lookup %q: %v", args.IfName, err)
		}
		containerInterface.Mac = link.Attrs().HardwareAddr.String()

		return nil
	}); err != nil {
		return err
	}

	if n.IsGW {
		var firstV4Addr net.IP
		// Set the IP address(es) on the bridge and enable forwarding
		for _, gws := range []*gwInfo{gwsV4, gwsV6} {
			for _, gw := range gws.gws {
				if gw.IP.To4() != nil && firstV4Addr == nil {
					firstV4Addr = gw.IP
				}

				err = ensureBridgeAddr(br, gws.family, &gw, n.ForceAddress)
				if err != nil {
					return fmt.Errorf("failed to set bridge addr: %v", err)
				}
			}

			if gws.gws != nil {
				if err = enableIPForward(gws.family); err != nil {
					return fmt.Errorf("failed to enable forwarding: %v", err)
				}
			}
		}

		if firstV4Addr != nil {
			if err := ip.SetHWAddrByIP(n.BrName, firstV4Addr, nil /* TODO IPv6 */); err != nil {
				return err
			}
		}
	}

	if n.IPMasq {
		chain := utils.FormatChainName(n.Name, args.ContainerID)
		comment := utils.FormatComment(n.Name, args.ContainerID)
		for _, ipc := range result.IPs {
			if err = ip.SetupIPMasq(ip.Network(&ipc.Address), chain, comment); err != nil {
				return err
			}
		}
	}

	// Refetch the bridge since its MAC address may change when the first
	// veth is added or after its IP address is set
	br, err = bridgeByName(n.BrName)
	if err != nil {
		return err
	}
	brInterface.Mac = br.Attrs().HardwareAddr.String()

	result.DNS = n.DNS

	// Bring up strongSwan
	if err = establishIpsec(args.Netns, args.ContainerID); err != nil {
		log.Println("strongswan", "failed to establish ipsec connection: %v", err)
	}

	return types.PrintResult(result, cniVersion)
}

// TODO: Rewrite this to avoid depend on binary ipsec and ip tool on the host
// We need a way to establish ipsec connection manually with strongswan
// Maybe need to look into libstrongswan
func establishIpsec(netNs string, containerId string) error {
	netNs = extractProcId(netNs)
	log.Println("strongswan", "establish ipsec for", netNs)

	os.Mkdir("/var/run/netns", os.ModePerm)
	os.Mkdir("/etc/ipsec.d/run", os.ModePerm)
	// Directory to hold charon pid file, this will be bindmount to /etc/ipsec.d/run in netowkr namespace
	os.MkdirAll("/etc/netns/ns-"+netNs+"/ipsec.d/run", os.ModePerm)

	os.Symlink(fmt.Sprintf("/proc/%s/ns/net", netNs), fmt.Sprintf("/var/run/netns/ns-%s", netNs))
	// Create ipsec.conf file
	//cp /etc/ipsec.client /etc/netns/ns-$pid/ipsec.conf
	//sed -i s/@leftid/@container-pid-$pid/ /etc/netns/ns-$pid/ipsec.conf
	// We use netNamesapce as leftid so that if container get kill, it gets namespace
	// of pause pods and will get same virtual ip
	ipsecConfContent := []byte(strings.Replace(ipsecConf, "@leftid", netNs, 1))
	os.Mkdir("/etc/netns/ns-"+netNs, os.ModePerm)
	if err := ioutil.WriteFile("/etc/netns/ns-"+netNs+"/ipsec.conf", ipsecConfContent, 0644); err != nil {
		return err
	}

	ipinfo, _ := exec.Command("ip", "netns", "exec", "/sbin/ip", "addr").Output()
	log.Println("strongswan", "ipaddr", string(ipinfo[:]))

	// Bringup ipsec
	args := []string{"bash", "-c", fmt.Sprintf("sleep 20; ip netns exec ns-%s ipsec start >>/tmp/cni-swan.log 2>&1", netNs), "&", "&>/tmp/nohup.log"}
	cmd := exec.Command("nohup", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return err
	}
	log.Println("strongswan", "ipsec result", out.String())
	//ip netns exec ns-$pid ipsec start
	return nil
}

// Stop ipsec, clearout namespace/configfile,symbol link that we have set
func teardownIpsec(netNs string) {
	netNs = extractProcId(netNs)
	log.Println("strongswan", "teardown ipsec for", netNs)
	exec.Command("ip", "netns", "exec", "ns-"+netNs, "ipsec", "stop").Run()
}

func cmdDel(args *skel.CmdArgs) error {
	n, _, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	if err := ipam.ExecDel(n.IPAM.Type, args.StdinData); err != nil {
		return err
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// First, let bring down the ipsec
	teardownIpsec(args.Netns)

	// so don't return an error if the device is already removed.
	// If the device isn't there then don't try to clean up IP masq either	.
	var ipn *net.IPNet
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		var err error
		ipn, err = ip.DelLinkByNameAddr(args.IfName, netlink.FAMILY_ALL)
		if err != nil && err == ip.ErrLinkNotFound {
			return nil
		}
		return err
	})

	if err != nil {
		return err
	}

	if ipn != nil && n.IPMasq {
		chain := utils.FormatChainName(n.Name, args.ContainerID)
		comment := utils.FormatComment(n.Name, args.ContainerID)
		err = ip.TeardownIPMasq(ipn, chain, comment)
	}

	return err
}

func extractProcId(netNs string) string {
	//Extract procid to and use its as namespace in symlink
	// /proc/27273/ns/net/ -> 27273
	part := strings.Split(netNs, "/")
	return part[2]
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}

const ipsecConf = `conn %default
	ikelifetime=60m
	keylife=20m
	rekeymargin=3m
	keyingtries=1
	keyexchange=ikev2
	authby=secret

conn home
	left=%any
	leftsourceip=%config
	leftid=@leftid
	leftfirewall=yes
	right=10.9.0.2
	rightsubnet=172.17.0.0/16,10.100.255.0/28
	rightid=server
	auto=start
`