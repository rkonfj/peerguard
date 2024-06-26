//go:build linux

package link

import (
	"net"

	"github.com/vishvananda/netlink"
)

func SetupLink(ifName, cidr string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}

	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}

	info.IPv4 = addr.IP.String()
	if addr.IP.To4() == nil {
		info.IPv6 = addr.IP.String()
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	return nil
}

func AddRoute(_ string, to *net.IPNet, via net.IP) error {
	return netlink.RouteAdd(&netlink.Route{
		Dst: to,
		Gw:  via,
	})
}

func DelRoute(_ string, to *net.IPNet, via net.IP) error {
	return netlink.RouteDel(&netlink.Route{
		Dst: to,
		Gw:  via,
	})
}
