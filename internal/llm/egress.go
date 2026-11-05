package llm

import (
	"errors"
	"net"
	"net/netip"
	"syscall"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// ErrEgressDenied identifies a destination rejected by the outbound policy.
var ErrEgressDenied = errors.New("llm: egress denied")

type egressPolicy struct {
	deny  []netip.Prefix
	allow []netip.Prefix
}

func newEgressPolicy(cfg config.EgressConfig) (egressPolicy, error) {
	var policy egressPolicy
	for _, group := range []struct {
		values []string
		out    *[]netip.Prefix
	}{
		{cfg.DenyCIDRs, &policy.deny},
		{cfg.AllowCIDRs, &policy.allow},
	} {
		for _, value := range group.values {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix.Addr().Is4In6() {
				return egressPolicy{}, errors.New("llm: invalid egress CIDR")
			}
			*group.out = append(*group.out, prefix.Masked())
		}
	}
	return policy, nil
}

func (p egressPolicy) check(ip netip.Addr) error {
	ip = ip.WithZone("").Unmap()
	for _, prefix := range p.deny {
		if prefix.Contains(ip) {
			return ErrEgressDenied
		}
	}
	for _, prefix := range p.allow {
		if prefix.Contains(ip) {
			return nil
		}
	}
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip == netip.MustParseAddr("255.255.255.255") {
		return ErrEgressDenied
	}
	return nil
}

func (p egressPolicy) control(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrEgressDenied
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ErrEgressDenied
	}
	return p.check(ip)
}
