package loginpage

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"time"
)

// interfaceAddrs lists this machine's addresses; a test replaces it.
var interfaceAddrs = net.InterfaceAddrs

// sansFor is the addresses a certificate for a page listening on host names:
// host itself, or for 0.0.0.0 and :: every address of this machine but
// loopback and link-local ones, in the order the system lists them.
func sansFor(host netip.Addr) ([]netip.Addr, error) {
	if !host.IsUnspecified() {
		return []netip.Addr{host.WithZone("")}, nil
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list this machine's addresses: %w", err)
	}
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, a := range addrs {
		var ip net.IP
		switch a := a.(type) {
		case *net.IPNet:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		default:
			continue
		}
		n, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		n = n.Unmap()
		if n.IsLoopback() || n.IsUnspecified() || n.IsLinkLocalUnicast() || n.IsMulticast() || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("this machine has no address off loopback to put in the page's certificate")
	}
	return out, nil
}

// newCert makes a self-signed ECDSA P-256 certificate for sans, valid from
// notBefore to notAfter, with its key in memory only, and says its
// fingerprint: the SHA-256 of its DER in colon-separated upper-case hex.
func newCert(sans []netip.Addr, notBefore, notAfter time.Time) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	ips := make([]net.IP, len(sans))
	for i, a := range sans {
		ips[i] = net.IP(a.AsSlice())
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rota login page"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, fingerprint(der), nil
}

// fingerprint is the SHA-256 of der in colon-separated upper-case hex, the
// way browsers show a certificate's.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	for i, c := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", c)
	}
	return b.String()
}
