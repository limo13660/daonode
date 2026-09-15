package sudoku

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	panel "github.com/limo13660/daonode/api/v2board"
)

type routePolicy struct {
	domains  []string
	prefixes []netip.Prefix
	ports    [][2]int
	blockAll bool
}

func compileRoutePolicy(routes []panel.Route) (*routePolicy, error) {
	p := &routePolicy{}
	for _, r := range routes {
		action := strings.ToLower(strings.TrimSpace(r.Action))
		blocked := action == "block" || action == "block_ip" || action == "block_port"
		if action == "route" || action == "route_ip" || action == "default_out" {
			blocked = routeBlocks(r.ActionValue)
		}
		if !blocked {
			continue
		}
		switch action {
		case "block", "route":
			for _, m := range r.Match {
				if v := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(m, "domain:"), "full:")); v != "" {
					p.domains = append(p.domains, strings.ToLower(strings.TrimSuffix(v, ".")))
				}
			}
		case "block_ip", "route_ip":
			for _, m := range r.Match {
				prefixes, err := compileIPPrefixes(m)
				if err != nil {
					return nil, fmt.Errorf("route %d %w", r.Id, err)
				}
				p.prefixes = append(p.prefixes, prefixes...)
			}
		case "block_port":
			for _, m := range r.Match {
				for _, item := range strings.Split(m, ",") {
					parts := strings.SplitN(strings.TrimSpace(item), "-", 2)
					from, err := strconv.Atoi(parts[0])
					if err != nil || from < 1 || from > 65535 {
						return nil, fmt.Errorf("route %d invalid port %q", r.Id, item)
					}
					to := from
					if len(parts) == 2 {
						to, err = strconv.Atoi(parts[1])
						if err != nil || to < from || to > 65535 {
							return nil, fmt.Errorf("route %d invalid port %q", r.Id, item)
						}
					}
					p.ports = append(p.ports, [2]int{from, to})
				}
			}
		default:
			if action == "default_out" {
				p.blockAll = true
			} else if action != "protocol" && action != "dns" {
				return nil, fmt.Errorf("route %d unsupported action %q", r.Id, r.Action)
			}
		}
	}
	return p, nil
}

func routeBlocks(value *string) bool {
	if value == nil {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	switch normalized {
	case "block", "blackhole":
		return true
	case "direct", "freedom":
		return false
	}
	var outbound struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal([]byte(*value), &outbound) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(outbound.Protocol)) {
	case "block", "blackhole":
		return true
	default:
		return false
	}
}

func compileIPPrefixes(raw string) ([]netip.Prefix, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if strings.EqualFold(value, "geoip:private") {
		// Keep this built-in matcher aligned with the other daonode kernels.
		values := []string{
			"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
			"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
			"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
			"224.0.0.0/4", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
		}
		prefixes := make([]netip.Prefix, 0, len(values))
		for _, item := range values {
			prefixes = append(prefixes, netip.MustParsePrefix(item))
		}
		return prefixes, nil
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "geoip:") {
		return nil, fmt.Errorf("external IP matcher %q is not supported", value)
	}
	if ip, err := netip.ParseAddr(value); err == nil {
		return []netip.Prefix{netip.PrefixFrom(ip, ip.BitLen())}, nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return nil, fmt.Errorf("invalid IP %q", value)
	}
	return []netip.Prefix{prefix}, nil
}

func (p *routePolicy) blocked(_ string, host string, port int) bool {
	if p == nil || p.blockAll {
		return p != nil && p.blockAll
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, d := range p.domains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		for _, prefix := range p.prefixes {
			if prefix.Contains(ip) {
				return true
			}
		}
	}
	for _, r := range p.ports {
		if port >= r[0] && port <= r[1] {
			return true
		}
	}
	return false
}
