package contentsearch

import (
	"context"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

const imageDNSBudget = 3 * time.Second
const imageHostTimeout = 1500 * time.Millisecond
const imageDNSWorkers = 4

// DNS lookups run once per hostname per search, with four workers and a shared
// three-second budget. Unreachable or private-resolving images are dropped; text
// sources stay usable. Import re-resolves and pins IPs in mediafetch separately.
func (c *Client) validateImageDNS(ctx context.Context, sources []Source) []Source {
	hosts := make(map[string]bool)
	for _, source := range sources {
		for _, image := range source.Images {
			if parsed, err := url.Parse(image.URL); err == nil {
				hosts[parsed.Hostname()] = false
			}
		}
	}
	if len(hosts) == 0 {
		return sources
	}
	dnsCtx, cancel := context.WithTimeout(ctx, imageDNSBudget)
	defer cancel()
	jobs := make(chan string, len(hosts))
	for host := range hosts {
		jobs <- host
	}
	close(jobs)
	var mu sync.Mutex
	var workers sync.WaitGroup
	for range min(imageDNSWorkers, len(hosts)) {
		workers.Go(func() {
			for host := range jobs {
				if dnsCtx.Err() != nil {
					return
				}
				lookupCtx, lookupCancel := context.WithTimeout(dnsCtx, imageHostTimeout)
				addresses, err := c.lookupNetIP(lookupCtx, "ip", host)
				lookupCancel()
				allowed := err == nil && len(addresses) > 0
				for _, address := range addresses {
					if !publicAddress(address) {
						allowed = false
						break
					}
				}
				mu.Lock()
				hosts[host] = allowed
				mu.Unlock()
			}
		})
	}
	workers.Wait()
	for i := range sources {
		images := make([]Image, 0, len(sources[i].Images))
		for _, image := range sources[i].Images {
			parsed, err := url.Parse(image.URL)
			if err == nil && hosts[parsed.Hostname()] {
				images = append(images, image)
			}
		}
		sources[i].Images = images
	}
	return sources
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}
