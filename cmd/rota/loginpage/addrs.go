package loginpage

import (
	"net"
	"net/netip"
)

// Choice is one address the page may listen on, with who can reach it there.
// Iface is the interface holding Addr, empty for 0.0.0.0 and for 127.0.0.1
// when no loopback interface lists it. Warn marks an address reachable
// beyond this machine's own networks.
type Choice struct {
	Addr, Iface, Who string
	Warn             bool
}

// iface is what choices needs to know about one network interface.
type iface struct {
	Name         string
	Up, Loopback bool
	Prefixes     []netip.Prefix
}

const (
	whoLoopback   = "only this machine; from another one, through ssh -L"
	whoEverywhere = "every interface: anyone who can reach this machine on any of them"
)

var (
	loopback4 = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	private   = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("fc00::/7"),
	}
	sharedNAT = netip.MustParsePrefix("100.64.0.0/10")
)

// Choices lists the addresses of this machine the page may listen on:
// 127.0.0.1 first, then the local networks, the carrier-grade NAT or
// Tailscale ones, any other address with a warning, and 0.0.0.0 last.
func Choices() ([]Choice, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	ifs := make([]iface, 0, len(nifs))
	for _, n := range nifs {
		addrs, err := n.Addrs()
		if err != nil {
			continue
		}
		f := iface{Name: n.Name, Up: n.Flags&net.FlagUp != 0, Loopback: n.Flags&net.FlagLoopback != 0}
		for _, a := range addrs {
			if p, ok := toPrefix(a); ok {
				f.Prefixes = append(f.Prefixes, p)
			}
		}
		ifs = append(ifs, f)
	}
	return choices(ifs), nil
}

func toPrefix(a net.Addr) (netip.Prefix, bool) {
	var ip net.IP
	bits := -1
	switch v := a.(type) {
	case *net.IPNet:
		ip = v.IP
		bits, _ = v.Mask.Size()
	case *net.IPAddr:
		ip = v.IP
	default:
		return netip.Prefix{}, false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	if bits < 0 || bits > addr.BitLen() {
		bits = addr.BitLen()
	}
	return netip.PrefixFrom(addr, bits), true
}

// choices is the pure part of Choices.
func choices(ifs []iface) []Choice {
	first := Choice{Addr: loopback4.String(), Who: whoLoopback}
	var local, shared, public []Choice
	seen := map[netip.Addr]bool{}
	for _, f := range ifs {
		if !f.Up {
			continue
		}
		for _, p := range f.Prefixes {
			a := p.Addr().Unmap().WithZone("")
			if f.Loopback && a == loopback4 && first.Iface == "" {
				first.Iface = f.Name
			}
			if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() ||
				a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || seen[a] {
				continue
			}
			seen[a] = true
			c := Choice{Addr: a.String(), Iface: f.Name}
			switch {
			case inAny(private, a):
				c.Who = "machines on the local network of " + f.Name
				local = append(local, c)
			case sharedNAT.Contains(a):
				c.Who = "machines on the carrier-grade NAT or Tailscale network of " + f.Name
				shared = append(shared, c)
			default:
				c.Who, c.Warn = "anyone on the internet who can reach "+f.Name, true
				public = append(public, c)
			}
		}
	}
	out := make([]Choice, 0, 2+len(local)+len(shared)+len(public))
	out = append(out, first)
	out = append(out, local...)
	out = append(out, shared...)
	out = append(out, public...)
	return append(out, Choice{Addr: netip.IPv4Unspecified().String(), Who: whoEverywhere, Warn: true})
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
