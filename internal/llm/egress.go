package llm

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

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

// providerUnavailable reports why a configured base URL cannot pass the
// policy at startup. A DNS lookup failure also fails closed: the provider is
// marked unavailable instead of stopping the whole process.
func providerUnavailable(baseURL string, policy egressPolicy) string {
	if strings.TrimSpace(baseURL) == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := parsed.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if policy.check(ip) != nil {
			return "egress_denied"
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return "dns_lookup_failed"
	}
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if !ok || policy.check(ip) != nil {
			return "egress_denied"
		}
	}
	return ""
}
