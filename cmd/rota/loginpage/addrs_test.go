package loginpage

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func prefixes(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

func TestLoopbackComesFirstAndEverywhereLast(t *testing.T) {
	ifs := []iface{
		{Name: "lo", Up: true, Loopback: true, Prefixes: prefixes(t, "127.0.0.1/8", "::1/128")},
		{Name: "eth0", Up: true, Prefixes: prefixes(t, "192.168.1.20/24", "fe80::1/64", "169.254.3.3/16")},
		{Name: "tailscale0", Up: true, Prefixes: prefixes(t, "100.101.1.2/32")},
		{Name: "en0", Up: true, Prefixes: prefixes(t, "203.0.113.7/24")},
		{Name: "en1", Up: false, Prefixes: prefixes(t, "10.0.0.5/24")},
	}
	want := []Choice{
		{Addr: "127.0.0.1", Iface: "lo", Who: "only this machine; from another one, through ssh -L"},
		{Addr: "192.168.1.20", Iface: "eth0", Who: "machines on the local network of eth0"},
		{Addr: "100.101.1.2", Iface: "tailscale0", Who: "machines on the carrier-grade NAT or Tailscale network of tailscale0"},
		{Addr: "203.0.113.7", Iface: "en0", Who: "anyone on the internet who can reach en0", Warn: true},
		{Addr: "0.0.0.0", Who: "every interface: anyone who can reach this machine on any of them", Warn: true},
	}
	if got := choices(ifs); !reflect.DeepEqual(got, want) {
		t.Fatalf("choices:\n got %+v\nwant %+v", got, want)
	}
}

func TestEachKindKeepsItsPlaceWhateverTheInterfaceOrder(t *testing.T) {
	ifs := []iface{
		{Name: "en0", Up: true, Prefixes: prefixes(t, "2001:db8::7/64", "203.0.113.7/24")},
		{Name: "tailscale0", Up: true, Prefixes: prefixes(t, "100.101.1.2/32", "fd7a:115c:a1e0::1/48")},
		{Name: "eth0", Up: true, Prefixes: prefixes(t, "10.1.2.3/8", "172.16.0.9/12", "ff02::1/16", "224.0.0.1/4")},
		{Name: "eth1", Up: true, Prefixes: prefixes(t, "10.1.2.3/8", "0.0.0.0/0", "::/0")},
		{Name: "docker0", Up: true, Prefixes: prefixes(t, "127.0.0.2/8")},
	}
	want := []Choice{
		{Addr: "127.0.0.1", Who: "only this machine; from another one, through ssh -L"},
		{Addr: "fd7a:115c:a1e0::1", Iface: "tailscale0", Who: "machines on the local network of tailscale0"},
		{Addr: "10.1.2.3", Iface: "eth0", Who: "machines on the local network of eth0"},
		{Addr: "172.16.0.9", Iface: "eth0", Who: "machines on the local network of eth0"},
		{Addr: "100.101.1.2", Iface: "tailscale0", Who: "machines on the carrier-grade NAT or Tailscale network of tailscale0"},
		{Addr: "2001:db8::7", Iface: "en0", Who: "anyone on the internet who can reach en0", Warn: true},
		{Addr: "203.0.113.7", Iface: "en0", Who: "anyone on the internet who can reach en0", Warn: true},
		{Addr: "0.0.0.0", Who: "every interface: anyone who can reach this machine on any of them", Warn: true},
	}
	if got := choices(ifs); !reflect.DeepEqual(got, want) {
		t.Fatalf("choices:\n got %+v\nwant %+v", got, want)
	}
}

func TestNoInterfacesStillOffersLoopbackAndEverywhere(t *testing.T) {
	got := choices(nil)
	if len(got) != 2 || got[0].Addr != "127.0.0.1" || got[0].Warn || got[1].Addr != "0.0.0.0" || !got[1].Warn {
		t.Fatalf("choices(nil) = %+v", got)
	}
}

func TestAddressesFromTheSystemBecomePrefixes(t *testing.T) {
	cases := []struct {
		in   net.Addr
		want string
	}{
		{&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)}, "192.168.1.20/24"},
		{&net.IPNet{IP: net.ParseIP("192.168.1.20").To4(), Mask: net.CIDRMask(24, 32)}, "192.168.1.20/24"},
		{&net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)}, "fd00::5/64"},
		{&net.IPAddr{IP: net.ParseIP("100.101.1.2")}, "100.101.1.2/32"},
	}
	for _, c := range cases {
		p, ok := toPrefix(c.in)
		if !ok || p.String() != c.want {
			t.Errorf("toPrefix(%v) = %v, %v; want %s", c.in, p, ok, c.want)
		}
	}
	if _, ok := toPrefix(&net.UnixAddr{Name: "x", Net: "unix"}); ok {
		t.Error("a unix address became a prefix")
	}
}

func TestChoicesOnThisMachine(t *testing.T) {
	got, err := Choices()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("Choices() = %+v, want at least loopback and every interface", got)
	}
	if got[0].Addr != "127.0.0.1" || got[0].Warn {
		t.Errorf("first = %+v, want 127.0.0.1 without a warning", got[0])
	}
	if last := got[len(got)-1]; last.Addr != "0.0.0.0" || !last.Warn {
		t.Errorf("last = %+v, want 0.0.0.0 with a warning", last)
	}
	for _, c := range got {
		a, err := netip.ParseAddr(c.Addr)
		if err != nil {
			t.Errorf("%q does not parse: %v", c.Addr, err)
			continue
		}
		if a.Zone() != "" || a.IsLinkLocalUnicast() || a.IsMulticast() || a == netip.IPv6Loopback() {
			t.Errorf("%s is offered and should not be", c.Addr)
		}
		if c.Who == "" {
			t.Errorf("%s says nobody about who can reach it", c.Addr)
		}
	}
}
