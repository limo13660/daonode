package singbox

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	panel "github.com/limo13660/daonode/api/v2board"
)

// buildRouteConfig translates the v2board route-group model into official
// sing-box options.  This keeps route groups a panel concern: individual
// kernel adapters only translate the common route representation once.
func buildRouteConfig(routes []panel.Route) (map[string]any, map[string]any, error) {
	routeConfig := map[string]any{"final": "direct"}
	if len(routes) == 0 {
		return routeConfig, nil, nil
	}

	rules := make([]any, 0, len(routes))
	dnsServers := []any{map[string]any{"type": "local", "tag": "daonode-local-dns"}}
	dnsRules := make([]any, 0)
	dnsConfig := map[string]any{"servers": dnsServers, "final": "daonode-local-dns"}
	usesGeoIP := false
	usesGeoSite := false
	usesProtocolSniffing := false

	for _, panelRoute := range routes {
		action := strings.ToLower(strings.TrimSpace(panelRoute.Action))
		var rule map[string]any
		var err error

		switch action {
		case "block", "route":
			rule, usesGeoSite, err = compileDomainRule(panelRoute.Match, usesGeoSite)
			if err == nil {
				rule["outbound"], err = routeOutbound(action, panelRoute.ActionValue)
			}
		case "block_ip", "route_ip":
			rule, usesGeoIP, err = compileIPRule(panelRoute.Match, usesGeoIP)
			if err == nil {
				if !hasRouteMatcher(rule) {
					continue
				}
				rule["outbound"], err = routeOutbound(action, panelRoute.ActionValue)
			}
		case "block_port":
			rule, err = compilePortRule(panelRoute.Match)
			if err == nil {
				if !hasRouteMatcher(rule) {
					continue
				}
				rule["outbound"] = "block"
			}
		case "protocol":
			usesProtocolSniffing = true
			rule, err = compileProtocolRule(panelRoute.Match)
			if err == nil {
				if !hasRouteMatcher(rule) {
					continue
				}
				rule["outbound"] = "block"
			}
		case "default_out":
			rule = map[string]any{}
			rule["outbound"], err = routeOutbound(action, panelRoute.ActionValue)
		case "dns":
			var dnsRule map[string]any
			dnsRule, usesGeoSite, err = compileDomainRule(panelRoute.Match, usesGeoSite)
			if err == nil {
				server, port, parseErr := parseDNSServer(panelRoute.ActionValue)
				if parseErr != nil {
					err = parseErr
				} else {
					tag := fmt.Sprintf("daonode-route-dns-%d", panelRoute.Id)
					dnsServers = append(dnsServers, map[string]any{
						"type":        "udp",
						"tag":         tag,
						"server":      server,
						"server_port": port,
					})
					if hasRouteMatcher(dnsRule) {
						dnsRule["server"] = tag
						dnsRules = append(dnsRules, dnsRule)
					} else {
						dnsConfig["final"] = tag
					}
				}
			}
			if err == nil {
				continue
			}
		default:
			err = fmt.Errorf("unsupported action %q", panelRoute.Action)
		}

		if err != nil {
			return nil, nil, fmt.Errorf("route %d (%s): %w", panelRoute.Id, panelRoute.Action, err)
		}
		rules = append(rules, rule)
	}

	if usesProtocolSniffing {
		// Panel protocol rules (HTTP, TLS, QUIC and BitTorrent) need a
		// metadata sniff before sing-box can evaluate them.
		rules = append([]any{map[string]any{"action": "sniff"}}, rules...)
	}
	if len(rules) > 0 {
		routeConfig["rules"] = rules
	}
	if usesGeoIP {
		routeConfig["geoip"] = map[string]any{"path": geoDataPath("DAONODE_GEOIP_PATH", "geoip.dat")}
	}
	if usesGeoSite {
		routeConfig["geosite"] = map[string]any{"path": geoDataPath("DAONODE_GEOSITE_PATH", "geosite.dat")}
	}
	if len(dnsRules) == 0 && len(dnsServers) == 1 {
		return routeConfig, nil, nil
	}
	dnsConfig["servers"] = dnsServers
	if len(dnsRules) > 0 {
		dnsConfig["rules"] = dnsRules
	}
	return routeConfig, dnsConfig, nil
}

func compileDomainRule(values []string, usesGeoSite bool) (map[string]any, bool, error) {
	rule := make(map[string]any)
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		switch {
		case strings.HasPrefix(lower, "geosite:"):
			if err := appendStringRuleValue(rule, "geosite", value[len("geosite:"):]); err != nil {
				return nil, usesGeoSite, err
			}
			usesGeoSite = true
		case strings.HasPrefix(lower, "domain:"):
			if err := appendStringRuleValue(rule, "domain_suffix", value[len("domain:"):]); err != nil {
				return nil, usesGeoSite, err
			}
		case strings.HasPrefix(lower, "full:"):
			if err := appendStringRuleValue(rule, "domain", value[len("full:"):]); err != nil {
				return nil, usesGeoSite, err
			}
		case strings.HasPrefix(lower, "regexp:"):
			if err := appendStringRuleValue(rule, "domain_regex", value[len("regexp:"):]); err != nil {
				return nil, usesGeoSite, err
			}
		case strings.HasPrefix(lower, "keyword:"):
			if err := appendStringRuleValue(rule, "domain_keyword", value[len("keyword:"):]); err != nil {
				return nil, usesGeoSite, err
			}
		case strings.HasPrefix(lower, "ext:"):
			return nil, usesGeoSite, fmt.Errorf("external domain lists are not available: %s", value)
		default:
			if err := appendStringRuleValue(rule, "domain_keyword", lower); err != nil {
				return nil, usesGeoSite, err
			}
		}
	}
	return rule, usesGeoSite, nil
}

func compileIPRule(values []string, usesGeoIP bool) (map[string]any, bool, error) {
	rule := make(map[string]any)
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		switch {
		case lower == "geoip:private":
			rule["ip_is_private"] = true
		case strings.HasPrefix(lower, "geoip:"):
			if err := appendStringRuleValue(rule, "geoip", value[len("geoip:"):]); err != nil {
				return nil, usesGeoIP, err
			}
			usesGeoIP = true
		default:
			if ip := net.ParseIP(value); ip != nil {
				if err := appendStringRuleValue(rule, "ip_cidr", ip.String()); err != nil {
					return nil, usesGeoIP, err
				}
				continue
			}
			if _, _, err := net.ParseCIDR(value); err != nil {
				return nil, usesGeoIP, fmt.Errorf("invalid IP or CIDR %q", value)
			}
			if err := appendStringRuleValue(rule, "ip_cidr", value); err != nil {
				return nil, usesGeoIP, err
			}
		}
	}
	return rule, usesGeoIP, nil
}

func compilePortRule(values []string) (map[string]any, error) {
	rule := make(map[string]any)
	ports := make([]int, 0)
	ranges := make([]string, 0)
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
			if len(parts) == 1 {
				ports = append(ports, from)
				continue
			}
			to, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || to < from || to > 65535 {
				return nil, fmt.Errorf("invalid port range %q", value)
			}
			ranges = append(ranges, fmt.Sprintf("%d:%d", from, to))
		}
	}
	if len(ports) > 0 {
		rule["port"] = ports
	}
	if len(ranges) > 0 {
		rule["port_range"] = ranges
	}
	return rule, nil
}

func compileProtocolRule(values []string) (map[string]any, error) {
	protocols := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		switch value {
		case "http", "tls", "quic", "bittorrent":
			protocols = append(protocols, value)
		default:
			return nil, fmt.Errorf("unsupported protocol matcher %q", raw)
		}
	}
	rule := make(map[string]any)
	if len(protocols) > 0 {
		rule["protocol"] = protocols
	}
	return rule, nil
}

func routeOutbound(action string, value *string) (string, error) {
	if action == "block" || action == "block_ip" {
		return "block", nil
	}
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
		return "", fmt.Errorf("Xray outbound protocol %q is not supported; use freedom or blackhole", outbound.Protocol)
	}
}

func parseDNSServer(value *string) (string, int, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", 0, fmt.Errorf("DNS server is empty")
	}
	server := strings.TrimSpace(*value)
	if strings.Contains(server, "://") {
		return "", 0, fmt.Errorf("DNS URL schemes are not supported: %s", server)
	}
	if host, port, err := net.SplitHostPort(server); err == nil {
		parsedPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || parsedPort < 1 || parsedPort > 65535 || host == "" {
			return "", 0, fmt.Errorf("invalid DNS server address %q", server)
		}
		return host, parsedPort, nil
	}
	if ip := net.ParseIP(server); ip != nil {
		return ip.String(), 53, nil
	}
	if strings.Contains(server, ":") {
		return "", 0, fmt.Errorf("invalid DNS server address %q", server)
	}
	return server, 53, nil
}

func appendStringRuleValue(rule map[string]any, key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s matcher is empty", key)
	}
	if key == "domain" || key == "domain_suffix" {
		value = strings.TrimSuffix(strings.ToLower(value), ".")
	}
	if values, ok := rule[key].([]string); ok {
		rule[key] = append(values, value)
	} else {
		rule[key] = []string{value}
	}
	return nil
}

func hasRouteMatcher(rule map[string]any) bool {
	return len(rule) > 0
}

func geoDataPath(envName, filename string) string {
	// The installer stores v2node-compatible data here.  The environment
	// override mirrors the Mieru adapter so both kernels use the same files.
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value
	}
	return "/etc/daonode/" + filename
}
