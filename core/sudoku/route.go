package sudoku

import (
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
			blocked = r.ActionValue != nil && strings.EqualFold(strings.TrimSpace(*r.ActionValue), "block")
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
				prefix, err := netip.ParsePrefix(strings.TrimSpace(m))
				if err != nil {
					if ip, e := netip.ParseAddr(strings.TrimSpace(m)); e == nil {
						prefix = netip.PrefixFrom(ip, ip.BitLen())
					} else {
						return nil, fmt.Errorf("route %d invalid IP %q", r.Id, m)
					}
				}
				p.prefixes = append(p.prefixes, prefix)
			}
		case "block_port":
			for _, m := range r.Match {
				parts := strings.SplitN(strings.TrimSpace(m), "-", 2)
				from, err := strconv.Atoi(parts[0])
				if err != nil || from < 1 || from > 65535 {
					return nil, fmt.Errorf("route %d invalid port %q", r.Id, m)
				}
				to := from
				if len(parts) == 2 {
					to, err = strconv.Atoi(parts[1])
					if err != nil || to < from || to > 65535 {
						return nil, fmt.Errorf("route %d invalid port %q", r.Id, m)
					}
				}
				p.ports = append(p.ports, [2]int{from, to})
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
