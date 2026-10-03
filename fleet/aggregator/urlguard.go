package aggregator

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The aggregator dereferences URLs that arrive inside heartbeats: openapi_url is
// fetched to build the contract registry, and the log fan-out derives a service's
// dashboard address from it and sends the ServiceToken there. A heartbeat is
// unauthenticated unless IngestToken is configured, so without a guard anyone who
// can reach the ingest port can aim the aggregator at an address of their choice
// (server-side request forgery) and, for the fan-out, receive the token.
//
// Private and loopback targets are deliberately still allowed: services living
// on RFC 1918 addresses is the normal deployment. What is refused is the set of
// addresses that are never a service — link-local (which covers the cloud
// metadata endpoint 169.254.169.254), unspecified and multicast — plus
// non-http(s) schemes and embedded credentials. FetchAllowedHosts narrows
// further when the operator knows the fleet.

var numericHost = regexp.MustCompile(`^(0[xX][0-9a-fA-F]+|[0-9]+)(\.(0[xX][0-9a-fA-F]+|[0-9]+))*$`)

var blockedHostnames = map[string]struct{}{
	"metadata.google.internal": {},
	"metadata":                 {},
	"instance-data":            {},
}

// lookupIPs is replaced in tests.
var lookupIPs = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

func forbiddenIP(ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// Alibaba Cloud's metadata endpoint sits in the CGNAT range, which cannot be
	// blocked wholesale (Tailscale and some clusters use it legitimately).
	return ip.Equal(net.IPv4(100, 100, 100, 200))
}

// checkFetchURL reports why the aggregator must not fetch raw, or nil.
func checkFetchURL(raw string, allowedHosts []string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("unparseable url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url scheme must be http or https")
	}
	if u.User != nil {
		return errors.New("url must not embed credentials")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errors.New("url has no host")
	}
	if len(allowedHosts) > 0 {
		ok := false
		for _, h := range allowedHosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == host || h == strings.ToLower(u.Host) {
				ok = true
				break
			}
		}
		if !ok {
			return errors.New("host is not in the aggregator's allowed hosts")
		}
	}
	if _, bad := blockedHostnames[host]; bad {
		return errors.New("host is a cloud metadata name")
	}
	if ip := net.ParseIP(host); ip != nil {
		if forbiddenIP(ip) {
			return errors.New("address is link-local, unspecified or multicast")
		}
		return nil
	}
	// "2852039166" and "0xA9FEA9FE" are not IPs to net.ParseIP but are to a C
	// resolver, and both mean 169.254.169.254.
	if numericHost.MatchString(host) {
		return errors.New("numeric host forms are not accepted")
	}
	// Resolve and check what the name points at. This narrows DNS tricks but is
	// not airtight (the name can change between this check and the dial); it is
	// the reason the allowlist exists.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ips, err := lookupIPs(ctx, host)
	if err != nil {
		return nil // unresolvable: the fetch itself will fail
	}
	for _, ip := range ips {
		if forbiddenIP(ip) {
			return errors.New("host resolves to a link-local, unspecified or multicast address")
		}
	}
	return nil
}
