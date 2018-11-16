// Copyright 2015 CNI authors
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
        "regexp"
        "runtime"
        "strconv"
        "time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
        "github.com/google/goexpect"
	"github.com/vishvananda/netlink"
)

type NetConf struct {
	types.NetConf

	// support chaining for master interface and IP decisions
	// occurring prior to running veth plugin
	RawPrevResult *map[string]interface{} `json:"prevResult"`
	PrevResult    *current.Result         `json:"-"`

	MTU    int    `json:"mtu"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(bytes []byte) (*NetConf, string, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}
	// Parse previous result
	if n.RawPrevResult != nil {
		resultBytes, err := json.Marshal(n.RawPrevResult)
		if err != nil {
			return nil, "", fmt.Errorf("could not serialize prevResult: %v", err)
		}
		res, err := version.NewResult(n.CNIVersion, resultBytes)
		if err != nil {
			return nil, "", fmt.Errorf("could not parse prevResult: %v", err)
		}
		n.RawPrevResult = nil
		n.PrevResult, err = current.NewResultFromResult(res)
		if err != nil {
			return nil, "", fmt.Errorf("could not convert result to current version: %v", err)
		}
	}
	return n, n.CNIVersion, nil
}

func createVeth(conf *NetConf, ifName string, netns ns.NetNS) (*current.Interface, error) {
	veth := &current.Interface{}

	// due to kernel bug we have to create with tmpname or it might
	// collide with the name on the host and error out
	tmpName, err := ip.RandomVethName()
	if err != nil {
		return nil, err
	}
	tmpPeerName, err := ip.RandomVethName()
	if err != nil {
		return nil, err
	}

        defaultNs, err := ns.GetCurrentNS()
        if err != nil {
                return nil,err
        }
        defer defaultNs.Close()

        la := netlink.NewLinkAttrs()
        la.Name = tmpName
        la.MTU = conf.MTU
        la.Namespace = netlink.NsFd(int(defaultNs.Fd()))

        mv := &netlink.Veth{LinkAttrs: la, PeerName: tmpPeerName}

	if err := netlink.LinkAdd(mv); err != nil {
		return nil, fmt.Errorf("failed to create veth: %v", err)
	}

        dev, err := netlink.LinkByName(tmpName)
        if err != nil {
                return nil,fmt.Errorf("failed to find interface %q: %v", tmpName, err)
        }

        if err := netlink.LinkSetNsFd(dev, int(netns.Fd())); err != nil {
                return nil,fmt.Errorf("failed to move %q to container netns: %v", dev.Attrs().Alias, err)
        }

	err = netns.Do(func(_ ns.NetNS) error {
                err := ip.RenameLink(tmpName, ifName)
                if err != nil {
                        return fmt.Errorf("failed to rename veth to %q: %v", ifName, err)
                }
		veth.Name = ifName

		// Re-fetch veth to get all properties/attributes
		contVeth, err := netlink.LinkByName(veth.Name)
		if err != nil {
			return fmt.Errorf("failed to refetch veth %q: %v", veth.Name, err)
		}
		// set alias name to link the interface in the container to the vpp host-interface
                err = netlink.LinkSetAlias(contVeth, tmpPeerName)
                if err != nil {
                        return fmt.Errorf("failed to alias veth to %q: %v", tmpPeerName, err)
                }
		veth.Mac = contVeth.Attrs().HardwareAddr.String()
		veth.Sandbox = netns.Path()

		return nil
	})
	if err != nil {
		return nil, err
	}

        dev, err = netlink.LinkByName(tmpPeerName)
        if err != nil {
                return nil,fmt.Errorf("failed to find interface %q: %v", tmpPeerName, err)
        }

        if err := netlink.LinkSetUp(dev); err != nil {
                return nil,fmt.Errorf("failed to set %q up: %v", dev.Attrs().Alias, err)
        }
        nspid := netlink.NsPid(int(netns.Fd()))
        alias := ifName + "-" + strconv.Itoa(int(nspid))
        err = netlink.LinkSetAlias(dev, alias)
        if err != nil {
                return nil,fmt.Errorf("failed to alias veth to %q: %v", alias, err)
        }

	vppaddress := ""
        if conf.VPP.Address != "" {
		vppaddress = conf.VPP.Address
                log.Printf("configured VPP address: %s", vppaddress)
	} else {
		vppaddress = "/opt/cni/bin/telnet 0 5002"
        }

        e, _, err := expect.Spawn(vppaddress, -1)
        if err != nil {
                return nil,fmt.Errorf("failed to connect to vpp: %v", err)
        }
        defer e.Close()

        timeout := 10 * time.Second
        vppRE := regexp.MustCompile("vpp# ")
        e.Expect(vppRE, timeout)
        cmd := "create host-interface name " + tmpPeerName + " "
        e.Send(cmd + "\n")
        result, _, _ := e.Expect(vppRE, timeout)
        log.Printf("%s: result: %s\n", cmd, result)
        cmd = "set int state host-" + tmpPeerName + " up"
        e.Send(cmd + "\n")
        result, _, _ = e.Expect(vppRE, timeout)
        log.Printf("%s: result: %s\n", cmd, result)
        e.Send("quit\n")

	return veth, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	n, cniVersion, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	vethInterface, err := createVeth(n, args.IfName, netns)
	if err != nil {
		return err
	}

	var result *current.Result
	// Configure iface from PrevResult if we have IPs and an IPAM
	// block has not been configured
	if n.IPAM.Type == "" && n.PrevResult != nil && len(n.PrevResult.IPs) > 0 {
		result = n.PrevResult
	} else {
		// run the IPAM plugin and get back the config to apply
		r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
		// Convert whatever the IPAM result was into the current Result type
		result, err = current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		if len(result.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}
	}
	for _, ipc := range result.IPs {
		// All addresses belong to the veth interface
		ipc.Interface = current.Int(0)
	}

	result.Interfaces = []*current.Interface{vethInterface}

	err = netns.Do(func(_ ns.NetNS) error {
		return ipam.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	result.DNS = n.DNS

	return types.PrintResult(result, cniVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	n, _, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	// On chained invocation, IPAM block can be empty
	if n.IPAM.Type != "" {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
                dev, err := netlink.LinkByName(args.IfName)
                if err != nil {
                         return fmt.Errorf("failed to find interface %q: %v", args.IfName, err)
                }
                alias := dev.Attrs().Alias

		vppaddress := ""
	        if n.VPP.Address != "" {
			vppaddress = n.VPP.Address
			log.Printf("configured VPP address: %s", vppaddress)
		} else {
			vppaddress = "/opt/cni/bin/telnet 0 5002"
		}

                e, _, err := expect.Spawn(vppaddress, -1)
                if err != nil {
                        return fmt.Errorf("failed to connect to vpp: %v", err)
                }
                defer e.Close()
                timeout := 10 * time.Second
                vppRE := regexp.MustCompile("vpp# ")
                e.Expect(vppRE, timeout)
                cmd := "delete host-interface name " + alias
                e.Send(cmd + "\n")
                result, _, _ := e.Expect(vppRE, timeout)
                log.Printf("%s: result: %s\n", cmd, result)
                e.Send("quit\n")

		if err := ip.DelLinkByName(args.IfName); err != nil {
			if err != ip.ErrLinkNotFound {
				return err
			}
		}
		return nil
	})

	return err
}

func main() {
	// TODO: implement plugin version
	skel.PluginMain(cmdAdd, cmdGet, cmdDel, version.All, "TODO")
}

func cmdGet(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}
