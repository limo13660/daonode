package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enfein/mieru/v3/apis/model"

	panel "github.com/limo13660/daonode/api/v2board"
)

var errRouteBlocked = errors.New("connection blocked by route rule")

const udpDNSCacheTTL = 5 * time.Minute

type routeDecision uint8

const (
	routeDirect routeDecision = iota
	routeBlock
)

type routeEngine struct {
	rules       []compiledRoute
	udpDNSCache sync.Map
}

type udpDNSCacheEntry struct {
	ip        net.IP
	expiresAt time.Time
}

type compiledRoute struct {
	id        int
	action    string
	domains   []domainMatcher
	ipprefix  []*net.IPNet
	ports     []portRange
	protocols map[string]struct{}
	resolver  *net.Resolver
	decision  routeDecision
}

type routeTarget struct {
	host    string
	ip      net.IP
	port    int
	network string
}

type domainMatcher struct {
	kind  string
	value string
	re    *regexp.Regexp
}

type portRange struct {
	from int
	to   int
}

func newRouteEngine(routes []panel.Route) (*routeEngine, error) {
	engine := &routeEngine{rules: make([]compiledRoute, 0, len(routes))}
	for _, route := range routes {
		rule := compiledRoute{id: route.Id, action: strings.ToLower(strings.TrimSpace(route.Action))}
		var err error
		switch rule.action {
		case "dns":
			rule.domains, err = compileDomains(route.Match)
			if err == nil {
				if route.ActionValue == nil || strings.TrimSpace(*route.ActionValue) == "" {
					err = errors.New("DNS server is empty")
				} else {
					rule.resolver, err = newDNSResolver(*route.ActionValue)
				}
			}
		case "block", "route":
			rule.domains, err = compileDomains(route.Match)
			if err == nil {
				rule.decision, err = routeActionDecision(rule.action, route.ActionValue)
			}
		case "block_ip", "route_ip":
			rule.ipprefix, err = compileIPPrefixes(route.Match)
			if err == nil {
				rule.decision, err = routeActionDecision(rule.action, route.ActionValue)
			}
		case "block_port":
			rule.ports, err = compilePorts(route.Match)
			rule.decision = routeBlock
		case "protocol":
			rule.protocols, err = compileProtocols(route.Match)
			rule.decision = routeBlock
		case "default_out":
			rule.decision, err = routeActionDecision(rule.action, route.ActionValue)
		default:
			err = fmt.Errorf("unsupported action %q", route.Action)
		}
		if err != nil {
			return nil, fmt.Errorf("route %d (%s): %w", route.Id, route.Action, err)
		}
		engine.rules = append(engine.rules, rule)
	}
	return engine, nil
}

func routeActionDecision(action string, value *string) (routeDecision, error) {
	if action == "block" || action == "block_ip" || action == "block_port" || action == "protocol" {
		return routeBlock, nil
	}
	if value == nil || strings.TrimSpace(*value) == "" {
		return routeDirect, errors.New("outbound config is empty")
	}
	plain := strings.ToLower(strings.TrimSpace(*value))
	switch plain {
	case "freedom", "direct":
		return routeDirect, nil
	case "blackhole", "block":
		return routeBlock, nil
	}
	var outbound struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(*value), &outbound); err != nil {
		return routeDirect, fmt.Errorf("decode outbound config: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(outbound.Protocol)) {
	case "freedom", "direct":
		return routeDirect, nil
	case "blackhole", "block":
		return routeBlock, nil
	default:
		return routeDirect, fmt.Errorf("Xray outbound protocol %q is not supported by the native daonode router; use freedom or blackhole", outbound.Protocol)
	}
}

func (r *routeEngine) dialTCP(ctx context.Context, addr model.AddrSpec) (net.Conn, error) {
	target := targetFromAddr("tcp", addr)
	if r.decision(target) == routeBlock {
		return nil, errRouteBlocked
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if target.host != "" {
		dialer.Resolver = r.resolverForHost(target.host)
	}
	return dialer.DialContext(ctx, "tcp", addr.String())
}

func (r *routeEngine) resolveUDPAddr(ctx context.Context, addr model.AddrSpec, payload []byte) (*net.UDPAddr, error) {
	target := targetFromAddr("udp", addr)
	if r.decisionWithProtocol(target, sniffProtocol("udp", payload)) == routeBlock {
		return nil, errRouteBlocked
	}
	if target.ip != nil {
		return &net.UDPAddr{IP: target.ip, Port: target.port}, nil
	}
	if ip, ok := r.cachedUDPIP(target.host, time.Now()); ok {
		return &net.UDPAddr{IP: ip, Port: target.port}, nil
	}
	resolver := r.resolverForHost(target.host)
	ips, err := resolver.LookupIP(ctx, "ip", target.host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, &net.AddrError{Err: "IP address not found", Addr: target.host}
	}
	r.storeUDPIP(target.host, ips[0], time.Now())
	return &net.UDPAddr{IP: ips[0], Port: target.port}, nil
}

func (r *routeEngine) cachedUDPIP(host string, now time.Time) (net.IP, bool) {
	key := normalizeDNSCacheHost(host)
	if key == "" {
		return nil, false
	}
	value, ok := r.udpDNSCache.Load(key)
	if !ok {
		return nil, false
	}
	entry := value.(udpDNSCacheEntry)
	if !now.Before(entry.expiresAt) {
		r.udpDNSCache.Delete(key)
		return nil, false
	}
	return append(net.IP(nil), entry.ip...), true
}

func (r *routeEngine) storeUDPIP(host string, ip net.IP, now time.Time) {
	key := normalizeDNSCacheHost(host)
	if key == "" || ip == nil {
		return
	}
	r.udpDNSCache.Store(key, udpDNSCacheEntry{
		ip:        append(net.IP(nil), ip...),
		expiresAt: now.Add(udpDNSCacheTTL),
	})
}

func normalizeDNSCacheHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func (r *routeEngine) decision(target routeTarget) routeDecision {
	return r.decisionWithProtocol(target, "")
}

func (r *routeEngine) decisionWithProtocol(target routeTarget, protocol string) routeDecision {
	for _, rule := range r.rules {
		if rule.action == "dns" {
			continue
		}
		if rule.matches(target, protocol) {
			return rule.decision
		}
	}
	return routeDirect
}

func (r *routeEngine) resolverForHost(host string) *net.Resolver {
	for _, rule := range r.rules {
		if rule.action == "dns" && rule.matchesDomain(host) {
			return rule.resolver
		}
	}
	return net.DefaultResolver
}

func (r *routeEngine) hasTCPProtocolRules() bool {
	for _, rule := range r.rules {
		if rule.action == "protocol" {
			for _, protocol := range []string{"http", "tls", "bittorrent"} {
				if _, ok := rule.protocols[protocol]; ok {
					return true
				}
			}
		}
	}
	return false
}

func (r compiledRoute) matches(target routeTarget, protocol string) bool {
	switch r.action {
	case "block", "route":
		return r.matchesDomain(target.host)
	case "block_ip", "route_ip":
		for _, prefix := range r.ipprefix {
			if target.ip != nil && prefix.Contains(target.ip) {
				return true
			}
		}
	case "block_port":
		for _, port := range r.ports {
			if target.port >= port.from && target.port <= port.to {
				return true
			}
		}
	case "protocol":
		if protocol == "" {
			return false
		}
		_, ok := r.protocols[protocol]
		return ok
	case "default_out":
		return true
	}
	return false
}

func (r compiledRoute) matchesDomain(host string) bool {
	if len(r.domains) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	for _, matcher := range r.domains {
		if matcher.matches(host) {
			return true
		}
	}
	return false
}

func (m domainMatcher) matches(host string) bool {
	switch m.kind {
	case "domain":
		return host == m.value || strings.HasSuffix(host, "."+m.value)
	case "full":
		return host == m.value
	case "regexp":
		return m.re.MatchString(host)
	default:
		return strings.Contains(host, m.value)
	}
}

func compileDomains(values []string) ([]domainMatcher, error) {
	result := make([]domainMatcher, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		switch {
		case strings.HasPrefix(lower, "geosite:"):
			return nil, fmt.Errorf("geosite rules are not available without an external geosite database: %s", value)
		case strings.HasPrefix(lower, "domain:"):
			result = append(result, domainMatcher{kind: "domain", value: strings.TrimSuffix(strings.TrimPrefix(lower, "domain:"), ".")})
		case strings.HasPrefix(lower, "full:"):
			result = append(result, domainMatcher{kind: "full", value: strings.TrimSuffix(strings.TrimPrefix(lower, "full:"), ".")})
		case strings.HasPrefix(lower, "regexp:"):
			re, err := regexp.Compile(value[len("regexp:"):])
			if err != nil {
				return nil, err
			}
			result = append(result, domainMatcher{kind: "regexp", re: re})
		case strings.HasPrefix(lower, "keyword:"):
			result = append(result, domainMatcher{kind: "keyword", value: strings.TrimPrefix(lower, "keyword:")})
		case strings.HasPrefix(lower, "ext:"):
			return nil, fmt.Errorf("external domain lists are not available: %s", value)
		default:
			result = append(result, domainMatcher{kind: "keyword", value: lower})
		}
	}
	return result, nil
}

func compileIPPrefixes(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(value), "geoip:") {
			return nil, fmt.Errorf("geoip rules are not available without an external geoip database: %s", value)
		}
		if ip := net.ParseIP(value); ip != nil {
			bits := 128
			if ip.To4() != nil {
				ip = ip.To4()
				bits = 32
			}
			result = append(result, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, prefix, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("invalid IP or CIDR %q", value)
		}
		result = append(result, prefix)
	}
	return result, nil
}

func compilePorts(values []string) ([]portRange, error) {
	var result []portRange
	for _, raw := range values {
		for _, item := range strings.Split(raw, ",") {
			value := strings.TrimSpace(item)
			if value == "" {
				continue
			}
			parts := strings.SplitN(value, "-", 2)
			from, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", value)
			}
			to := from
			if len(parts) == 2 {
				to, err = strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil {
					return nil, fmt.Errorf("invalid port range %q", value)
				}
			}
			if from < 1 || to > 65535 || from > to {
				return nil, fmt.Errorf("port range %q is outside 1-65535", value)
			}
			result = append(result, portRange{from: from, to: to})
		}
	}
	return result, nil
}

func compileProtocols(values []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		switch value {
		case "http", "tls", "quic", "bittorrent":
			result[value] = struct{}{}
		default:
			return nil, fmt.Errorf("unsupported protocol matcher %q", raw)
		}
	}
	return result, nil
}

func newDNSResolver(server string) (*net.Resolver, error) {
	address, err := normalizeDNSServer(server)
	if err != nil {
		return nil, err
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, network, address)
		},
	}, nil
}

func normalizeDNSServer(server string) (string, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", errors.New("DNS server is empty")
	}
	if strings.Contains(server, "://") {
		return "", fmt.Errorf("DNS URL schemes are not supported: %s", server)
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server, nil
	}
	if ip := net.ParseIP(server); ip != nil {
		return net.JoinHostPort(ip.String(), "53"), nil
	}
	if strings.Contains(server, ":") {
		return "", fmt.Errorf("invalid DNS server address %q", server)
	}
	return net.JoinHostPort(server, "53"), nil
}

func targetFromAddr(network string, addr model.AddrSpec) routeTarget {
	return routeTarget{
		host:    strings.TrimSuffix(strings.ToLower(addr.FQDN), "."),
		ip:      addr.IP,
		port:    addr.Port,
		network: network,
	}
}

func sniffProtocol(network string, payload []byte) string {
	if network == "udp" {
		if len(payload) >= 5 && payload[0]&0x40 != 0 {
			return "quic"
		}
		return ""
	}
	if len(payload) >= 3 && payload[1] == 0x03 && payload[0] >= 0x14 && payload[0] <= 0x17 {
		return "tls"
	}
	if len(payload) >= 20 && payload[0] == 19 && string(payload[1:20]) == "BitTorrent protocol" {
		return "bittorrent"
	}
	upper := strings.ToUpper(string(payload))
	for _, prefix := range []string{"GET ", "POST ", "PUT ", "HEAD ", "DELETE ", "OPTIONS ", "PATCH ", "CONNECT ", "PRI * HTTP/2.0"} {
		if strings.HasPrefix(upper, prefix) {
			return "http"
		}
	}
	return ""
}
