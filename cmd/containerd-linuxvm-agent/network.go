//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The VM's own uplink: QEMU's user-mode network.
const (
	vmAddr    = "10.0.2.15/24"
	vmGateway = "10.0.2.2"
	vmDNS     = "10.0.2.3"
)

func setupVMNetwork() error {
	lo, err := netlink.LinkByName("lo")
	if err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	var uplink netlink.Link
	for _, l := range links {
		if l.Type() == "device" && l.Attrs().Name != "lo" {
			uplink = l
			break
		}
	}
	if uplink == nil {
		return fmt.Errorf("no network device")
	}
	addr, _ := netlink.ParseAddr(vmAddr)
	if err := netlink.AddrAdd(uplink, addr); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(uplink); err != nil {
		return err
	}
	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: uplink.Attrs().Index,
		Gw:        net.ParseIP(vmGateway),
	})
}

// bridgeNet is a Docker network: a Linux bridge, its subnet, NAT out of
// the VM, and a DNS server on the gateway that knows its containers.
type bridgeNet struct {
	name   string
	bridge string
	subnet *net.IPNet
	gw     net.IP
	used   map[uint32]string // host part -> container id
	dns    *dnsServer
}

type endpoint struct {
	net   *bridgeNet
	ip    net.IP
	veth  string
	names []string
}

type networks struct {
	mu        sync.Mutex
	nets      map[string]*bridgeNet
	members   map[string][]*endpoint // container id -> its endpoints
	upstreams []string               // DNS servers for names the networks do not know
}

func newNetworks() *networks {
	return &networks{
		nets:      make(map[string]*bridgeNet),
		members:   make(map[string][]*endpoint),
		upstreams: []string{vmDNS},
	}
}

// network returns the named network, creating it. n.mu is held.
func (n *networks) network(name string) (*bridgeNet, error) {
	if bn, ok := n.nets[name]; ok {
		return bn, nil
	}
	// Docker's default bridge is 172.17.0.0/16; the others follow it.
	taken := map[int]bool{}
	for _, bn := range n.nets {
		taken[int(bn.subnet.IP.To4()[1])] = true
	}
	second := -1
	if name == "bridge" && !taken[17] {
		second = 17
	} else {
		for s := 18; s < 32; s++ {
			if !taken[s] {
				second = s
				break
			}
		}
	}
	if second < 0 {
		return nil, fmt.Errorf("no subnet left for network %s", name)
	}
	subnet := &net.IPNet{IP: net.IPv4(172, byte(second), 0, 0).To4(), Mask: net.CIDRMask(16, 32)}
	gw := net.IPv4(172, byte(second), 0, 1).To4()
	brName := "docker0"
	if name != "bridge" {
		sum := sha256.Sum256([]byte(name))
		brName = "br-" + hex.EncodeToString(sum[:])[:12]
	}

	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: brName}}
	if err := netlink.LinkAdd(br); err != nil && !strings.Contains(err.Error(), "exists") {
		return nil, fmt.Errorf("create bridge %s: %w", brName, err)
	}
	link, err := netlink.LinkByName(brName)
	if err != nil {
		return nil, err
	}
	_ = netlink.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{IP: gw, Mask: subnet.Mask}})
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}
	// NAT to the outside, and no routing between different networks (as
	// Docker isolates them). Traffic within a bridge is switched, not
	// routed, except when it goes through the gateway address.
	rules := [][]string{
		{"-t", "nat", "-A", "POSTROUTING", "-s", subnet.String(), "!", "-o", brName, "-j", "MASQUERADE"},
		{"-A", "FORWARD", "-i", brName, "-o", brName, "-j", "ACCEPT"},
		{"-A", "FORWARD", "-i", brName, "-o", "br-+", "-j", "DROP"},
	}
	if brName != "docker0" {
		rules = append(rules, []string{"-A", "FORWARD", "-i", brName, "-o", "docker0", "-j", "DROP"})
	}
	for _, rule := range rules {
		if err := run("iptables", rule...); err != nil {
			logf("network %s: %v", name, err)
		}
	}

	bn := &bridgeNet{
		name:   name,
		bridge: brName,
		subnet: subnet,
		gw:     gw,
		used:   map[uint32]string{1: ""},
	}
	bn.dns = startDNS(gw, n, bn)
	n.nets[name] = bn
	return bn, nil
}

func (bn *bridgeNet) allocate(id string) (net.IP, error) {
	base := binary.BigEndian.Uint32(bn.subnet.IP.To4())
	for host := uint32(2); host < 0xfffe; host++ {
		if _, ok := bn.used[host]; ok {
			continue
		}
		bn.used[host] = id
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+host)
		return ip, nil
	}
	return nil, fmt.Errorf("network %s is full", bn.name)
}

func (bn *bridgeNet) release(ip net.IP) {
	host := binary.BigEndian.Uint32(ip.To4()) - binary.BigEndian.Uint32(bn.subnet.IP.To4())
	delete(bn.used, host)
}

// setUpstreams takes the DNS servers of a container's resolv.conf (the
// Engine writes the host's) for the names the networks do not resolve.
func (n *networks) setUpstreams(servers []string) {
	var keep []string
	for _, s := range servers {
		ip := net.ParseIP(s)
		if ip == nil || ip.IsLoopback() {
			continue // the host's own resolver is not reachable from here
		}
		keep = append(keep, s)
	}
	keep = append(keep, vmDNS)
	n.mu.Lock()
	n.upstreams = keep
	n.mu.Unlock()
}

func (n *networks) upstreamServers() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.upstreams...)
}

// attach connects the container whose init is pid to its networks: a veth
// pair per network, its end in the container named eth0, eth1, ...
func (n *networks) attach(id string, pid int, cfg linuxvm.Network, names []string) ([]*endpoint, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var eps []*endpoint
	fail := func(err error) ([]*endpoint, error) {
		n.detachLocked(id, eps)
		return nil, err
	}
	for i, e := range cfg.Networks {
		bn, err := n.network(e.Name)
		if err != nil {
			return fail(err)
		}
		ip, err := bn.allocate(id)
		if err != nil {
			return fail(err)
		}
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		ep := &endpoint{
			net:   bn,
			ip:    ip,
			veth:  fmt.Sprintf("veth%s%d", short, i),
			names: append(append([]string(nil), names...), e.Aliases...),
		}
		eps = append(eps, ep)
		if err := plugVeth(ep, pid, i, bn); err != nil {
			return fail(fmt.Errorf("connect to network %s: %w", e.Name, err))
		}
	}
	n.members[id] = append(n.members[id], eps...)
	return eps, nil
}

func plugVeth(ep *endpoint, pid, index int, bn *bridgeNet) error {
	peer := fmt.Sprintf("tmp%d%d", pid, index)
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: ep.veth, MTU: 1500},
		PeerName:  peer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return err
	}
	host, err := netlink.LinkByName(ep.veth)
	if err != nil {
		return err
	}
	br, err := netlink.LinkByName(bn.bridge)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMaster(host, br); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return err
	}
	peerLink, err := netlink.LinkByName(peer)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetNsPid(peerLink, pid); err != nil {
		return err
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	link, err := h.LinkByName(peer)
	if err != nil {
		return err
	}
	if err := h.LinkSetName(link, fmt.Sprintf("eth%d", index)); err != nil {
		return err
	}
	if err := h.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{IP: ep.ip, Mask: bn.subnet.Mask}}); err != nil {
		return err
	}
	if err := h.LinkSetUp(link); err != nil {
		return err
	}
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	if index == 0 {
		return h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: bn.gw})
	}
	return nil
}

// detach releases a container's addresses and names.
func (n *networks) detach(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.detachLocked(id, n.members[id])
	delete(n.members, id)
}

func (n *networks) detachLocked(id string, eps []*endpoint) {
	for _, ep := range eps {
		ep.net.release(ep.ip)
		if l, err := netlink.LinkByName(ep.veth); err == nil {
			_ = netlink.LinkDel(l)
		}
	}
}

// ip is the address of a container on its first network.
func (n *networks) ip(id string) net.IP {
	n.mu.Lock()
	defer n.mu.Unlock()
	if eps := n.members[id]; len(eps) > 0 {
		return eps[0].ip
	}
	return nil
}

// lookup resolves a container name or alias on a network.
func (n *networks) lookup(bn *bridgeNet, name string) net.IP {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, eps := range n.members {
		for _, ep := range eps {
			if ep.net != bn {
				continue
			}
			for _, nm := range ep.names {
				if strings.ToLower(nm) == name {
					return ep.ip
				}
			}
		}
	}
	return nil
}
