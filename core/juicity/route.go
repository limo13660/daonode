package juicity

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	panel "github.com/limo13660/daonode/api/v2board"
)

type routePolicy struct {
	domains  []domainRule
	prefixes []netip.Prefix
	ports    []portRange
	blockAll bool
}

type domainRule struct {
	mode  string
	value string
	re    *regexp.Regexp
}

type portRange struct {
	from int
	to   int
}

func compileRoutePolicy(routes []panel.Route) (*routePolicy, error) {
	policy := &routePolicy{}
	for _, route := range routes {
		action := strings.ToLower(strings.TrimSpace(route.Action))
		blocked := false
		switch action {
		case "block", "block_ip", "block_port":
			blocked = true
		case "route", "route_ip", "default_out":
			outbound, err := routeOutbound(route.ActionValue)
			if err != nil {
				return nil, fmt.Errorf("route %d (%s): %w", route.Id, route.Action, err)
			}
			blocked = outbound == "block"
		case "protocol":
			return nil, fmt.Errorf("route %d: protocol sniffing is not supported by the official Juicity server", route.Id)
		case "dns":
			return nil, fmt.Errorf("route %d: custom DNS routes are not supported by the official Juicity server", route.Id)
		default:
			return nil, fmt.Errorf("route %d: unsupported action %q", route.Id, route.Action)
		}
		if !blocked {
			continue
		}
		switch action {
		case "block", "route":
			for _, value := range route.Match {
				rule, err := compileDomainRule(value)
				if err != nil {
					return nil, fmt.Errorf("route %d: %w", route.Id, err)
				}
				if rule != nil {
					policy.domains = append(policy.domains, *rule)
				}
			}
		case "block_ip", "route_ip":
			for _, value := range route.Match {
				prefixes, err := compileIPPrefixes(value)
				if err != nil {
					return nil, fmt.Errorf("route %d: %w", route.Id, err)
				}
				policy.prefixes = append(policy.prefixes, prefixes...)
			}
		case "block_port":
			ports, err := compilePortRanges(route.Match)
			if err != nil {
				return nil, fmt.Errorf("route %d: %w", route.Id, err)
			}
			policy.ports = append(policy.ports, ports...)
		case "default_out":
			policy.blockAll = true
		}
	}
	return policy, nil
}

func (p *routePolicy) blocked(_ string, host string, port int) bool {
	if p == nil {
		return false
	}
	if p.blockAll {
		return true
	}
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, rule := range p.domains {
		if rule.matches(normalizedHost) {
			return true
		}
	}
	if address, err := netip.ParseAddr(normalizedHost); err == nil {
		for _, prefix := range p.prefixes {
			if prefix.Contains(address) {
				return true
			}
		}
	}
	for _, blocked := range p.ports {
		if port >= blocked.from && port <= blocked.to {
			return true
		}
	}
	return false
}

func (r domainRule) matches(host string) bool {
	switch r.mode {
	case "exact":
		return host == r.value
	case "suffix":
		return host == r.value || strings.HasSuffix(host, "."+r.value)
	case "keyword":
		return strings.Contains(host, r.value)
	case "regexp":
		return r.re.MatchString(host)
	default:
		return false
	}
}

func compileDomainRule(raw string) (*domainRule, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "geosite:"), strings.HasPrefix(lower, "ext:"):
		return nil, fmt.Errorf("external domain matcher %q is not supported", value)
	case strings.HasPrefix(lower, "domain:"):
		domain := normalizeDomain(value[len("domain:"):])
		if domain == "" {
			return nil, fmt.Errorf("empty domain matcher")
		}
		return &domainRule{mode: "suffix", value: domain}, nil
	case strings.HasPrefix(lower, "full:"):
		domain := normalizeDomain(value[len("full:"):])
		if domain == "" {
			return nil, fmt.Errorf("empty full-domain matcher")
		}
		return &domainRule{mode: "exact", value: domain}, nil
	case strings.HasPrefix(lower, "regexp:"):
		expression := value[len("regexp:"):]
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return nil, fmt.Errorf("invalid domain regular expression %q", expression)
		}
		return &domainRule{mode: "regexp", re: compiled}, nil
	case strings.HasPrefix(lower, "keyword:"):
		keyword := strings.ToLower(strings.TrimSpace(value[len("keyword:"):]))
		if keyword == "" {
			return nil, fmt.Errorf("empty domain keyword")
		}
		return &domainRule{mode: "keyword", value: keyword}, nil
	default:
		return &domainRule{mode: "keyword", value: lower}, nil
	}
}

func compileIPPrefixes(raw string) ([]netip.Prefix, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if strings.EqualFold(value, "geoip:private") {
		values := []string{"10.0.0.0/8", "127.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fe80::/10"}
		result := make([]netip.Prefix, 0, len(values))
		for _, item := range values {
			result = append(result, netip.MustParsePrefix(item))
		}
		return result, nil
	}
	if strings.HasPrefix(strings.ToLower(value), "geoip:") {
		return nil, fmt.Errorf("external IP matcher %q is not supported", value)
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return []netip.Prefix{netip.PrefixFrom(address, address.BitLen())}, nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return nil, fmt.Errorf("invalid IP or CIDR %q", value)
	}
	return []netip.Prefix{prefix}, nil
}

func compilePortRanges(values []string) ([]portRange, error) {
	result := make([]portRange, 0)
	for _, raw := range values {
		for _, item := range strings.Split(raw, ",") {
			value := strings.TrimSpace(item)
			if value == "" {
				continue
			}
			parts := strings.SplitN(value, "-", 2)
			from, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil || from < 1 || from > 65535 {
				return nil, fmt.Errorf("invalid port %q", value)
			}
			to := from
			if len(parts) == 2 {
				to, err = strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil || to < from || to > 65535 {
					return nil, fmt.Errorf("invalid port range %q", value)
				}
			}
			result = append(result, portRange{from: from, to: to})
		}
	}
	return result, nil
}

func routeOutbound(value *string) (string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", fmt.Errorf("outbound config is empty")
	}
	switch strings.ToLower(strings.TrimSpace(*value)) {
	case "freedom", "direct":
		return "direct", nil
	case "blackhole", "block":
		return "block", nil
	}
	var outbound struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(*value), &outbound); err != nil {
		return "", fmt.Errorf("decode outbound config: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(outbound.Protocol)) {
	case "freedom", "direct":
		return "direct", nil
	case "blackhole", "block":
		return "block", nil
	default:
		return "", fmt.Errorf("outbound protocol %q is not supported", outbound.Protocol)
	}
}

func normalizeDomain(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
