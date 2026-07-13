package core

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
)

// geoDataStore reads the same length-delimited protobuf records used by the
// geoip.dat and geosite.dat files shipped with v2node. It intentionally keeps
// only the data needed by daonode's native route matcher.
type geoDataStore struct {
	geoIPPath   string
	geoSitePath string
	ipCache     sync.Map // country code -> []*net.IPNet
	siteCache   sync.Map // country code -> []domainMatcher
}

var (
	sharedGeoDataOnce sync.Once
	sharedGeoData     *geoDataStore
)

func sharedGeoDataStore() *geoDataStore {
	sharedGeoDataOnce.Do(func() {
		sharedGeoData = newGeoDataStore()
	})
	return sharedGeoData
}

func newGeoDataStore() *geoDataStore {
	return newGeoDataStoreWithPaths(defaultGeoDataPath("DAONODE_GEOIP_PATH", "geoip.dat"), defaultGeoDataPath("DAONODE_GEOSITE_PATH", "geosite.dat"))
}

func newGeoDataStoreWithPaths(geoIPPath, geoSitePath string) *geoDataStore {
	return &geoDataStore{geoIPPath: geoIPPath, geoSitePath: geoSitePath}
}

func defaultGeoDataPath(envName, filename string) string {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value
	}
	for _, candidate := range []string{
		filepath.Join("/etc/daonode", filename),
		filepath.Join("/usr/local/daonode", filename),
		filename,
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join("/etc/daonode", filename)
}

func (g *geoDataStore) loadIP(code string) ([]*net.IPNet, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, fmt.Errorf("GeoIP code is empty")
	}
	if cached, ok := g.ipCache.Load(code); ok {
		return cached.([]*net.IPNet), nil
	}
	data, err := os.ReadFile(g.geoIPPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", g.geoIPPath, err)
	}
	body, err := findGeoDataRecord(data, code)
	if err != nil {
		return nil, fmt.Errorf("load GeoIP %s: %w", code, err)
	}
	prefixes, err := parseGeoIPRecord(body)
	if err != nil {
		return nil, fmt.Errorf("decode GeoIP %s: %w", code, err)
	}
	g.ipCache.Store(code, prefixes)
	return prefixes, nil
}

func (g *geoDataStore) loadSite(code string) ([]domainMatcher, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, fmt.Errorf("GeoSite code is empty")
	}
	if cached, ok := g.siteCache.Load(code); ok {
		return cached.([]domainMatcher), nil
	}
	data, err := os.ReadFile(g.geoSitePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", g.geoSitePath, err)
	}
	body, err := findGeoDataRecord(data, code)
	if err != nil {
		return nil, fmt.Errorf("load GeoSite %s: %w", code, err)
	}
	domains, err := parseGeoSiteRecord(body)
	if err != nil {
		return nil, fmt.Errorf("decode GeoSite %s: %w", code, err)
	}
	g.siteCache.Store(code, domains)
	return domains, nil
}

func findGeoDataRecord(data []byte, code string) ([]byte, error) {
	for len(data) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return nil, fmt.Errorf("invalid record tag: %v", protowire.ParseError(tagLen))
		}
		data = data[tagLen:]
		if number != 1 || wireType != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(number, wireType, data)
			if skip < 0 {
				return nil, fmt.Errorf("invalid record field: %v", protowire.ParseError(skip))
			}
			data = data[skip:]
			continue
		}
		body, bodyLen := protowire.ConsumeBytes(data)
		if bodyLen < 0 {
			return nil, fmt.Errorf("invalid record body: %v", protowire.ParseError(bodyLen))
		}
		data = data[bodyLen:]
		recordCode, payload, err := splitGeoDataRecord(body)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(recordCode, code) {
			return payload, nil
		}
	}
	return nil, fmt.Errorf("code %s not found", code)
}

func splitGeoDataRecord(body []byte) (string, []byte, error) {
	number, wireType, tagLen := protowire.ConsumeTag(body)
	if tagLen < 0 || number != 1 || wireType != protowire.BytesType {
		return "", nil, fmt.Errorf("invalid record code field")
	}
	body = body[tagLen:]
	code, codeLen := protowire.ConsumeBytes(body)
	if codeLen < 0 {
		return "", nil, fmt.Errorf("invalid record code: %v", protowire.ParseError(codeLen))
	}
	return string(code), body[codeLen:], nil
}

func parseGeoIPRecord(data []byte) ([]*net.IPNet, error) {
	prefixes := make([]*net.IPNet, 0)
	for len(data) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return nil, fmt.Errorf("invalid GeoIP field: %v", protowire.ParseError(tagLen))
		}
		data = data[tagLen:]
		if number != 2 || wireType != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(number, wireType, data)
			if skip < 0 {
				return nil, fmt.Errorf("invalid GeoIP field: %v", protowire.ParseError(skip))
			}
			data = data[skip:]
			continue
		}
		cidr, cidrLen := protowire.ConsumeBytes(data)
		if cidrLen < 0 {
			return nil, fmt.Errorf("invalid GeoIP CIDR: %v", protowire.ParseError(cidrLen))
		}
		data = data[cidrLen:]
		prefix, err := parseGeoCIDR(cidr)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func parseGeoCIDR(data []byte) (*net.IPNet, error) {
	var ip net.IP
	var prefix uint64
	var prefixSet bool
	for len(data) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return nil, fmt.Errorf("invalid CIDR field: %v", protowire.ParseError(tagLen))
		}
		data = data[tagLen:]
		switch number {
		case 1:
			if wireType != protowire.BytesType {
				return nil, fmt.Errorf("CIDR IP has invalid wire type")
			}
			value, valueLen := protowire.ConsumeBytes(data)
			if valueLen < 0 {
				return nil, fmt.Errorf("invalid CIDR IP: %v", protowire.ParseError(valueLen))
			}
			ip = append(net.IP(nil), value...)
			data = data[valueLen:]
		case 2:
			if wireType != protowire.VarintType {
				return nil, fmt.Errorf("CIDR prefix has invalid wire type")
			}
			value, valueLen := protowire.ConsumeVarint(data)
			if valueLen < 0 {
				return nil, fmt.Errorf("invalid CIDR prefix: %v", protowire.ParseError(valueLen))
			}
			prefix, prefixSet = value, true
			data = data[valueLen:]
		default:
			skip := protowire.ConsumeFieldValue(number, wireType, data)
			if skip < 0 {
				return nil, fmt.Errorf("invalid CIDR field: %v", protowire.ParseError(skip))
			}
			data = data[skip:]
		}
	}
	if len(ip) != net.IPv4len && len(ip) != net.IPv6len {
		return nil, fmt.Errorf("CIDR IP must contain 4 or 16 bytes")
	}
	bits := 8 * len(ip)
	if !prefixSet || prefix > uint64(bits) {
		return nil, fmt.Errorf("CIDR prefix %d is outside %d-bit address", prefix, bits)
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(int(prefix), bits)}, nil
}

func parseGeoSiteRecord(data []byte) ([]domainMatcher, error) {
	domains := make([]domainMatcher, 0)
	for len(data) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return nil, fmt.Errorf("invalid GeoSite field: %v", protowire.ParseError(tagLen))
		}
		data = data[tagLen:]
		if number != 2 || wireType != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(number, wireType, data)
			if skip < 0 {
				return nil, fmt.Errorf("invalid GeoSite field: %v", protowire.ParseError(skip))
			}
			data = data[skip:]
			continue
		}
		domain, domainLen := protowire.ConsumeBytes(data)
		if domainLen < 0 {
			return nil, fmt.Errorf("invalid GeoSite domain: %v", protowire.ParseError(domainLen))
		}
		data = data[domainLen:]
		matcher, err := parseGeoDomain(domain)
		if err != nil {
			return nil, err
		}
		if matcher.value != "" {
			domains = append(domains, matcher)
		}
	}
	return domains, nil
}

func parseGeoDomain(data []byte) (domainMatcher, error) {
	matcher := domainMatcher{kind: "keyword"}
	for len(data) > 0 {
		number, wireType, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return domainMatcher{}, fmt.Errorf("invalid domain field: %v", protowire.ParseError(tagLen))
		}
		data = data[tagLen:]
		switch number {
		case 1:
			if wireType != protowire.VarintType {
				return domainMatcher{}, fmt.Errorf("domain type has invalid wire type")
			}
			typeValue, valueLen := protowire.ConsumeVarint(data)
			if valueLen < 0 {
				return domainMatcher{}, fmt.Errorf("invalid domain type: %v", protowire.ParseError(valueLen))
			}
			switch typeValue {
			case 0:
				matcher.kind = "keyword"
			case 1:
				matcher.kind = "regexp"
			case 2:
				matcher.kind = "domain"
			case 3:
				matcher.kind = "full"
			default:
				return domainMatcher{}, fmt.Errorf("unsupported domain type %d", typeValue)
			}
			data = data[valueLen:]
		case 2:
			if wireType != protowire.BytesType {
				return domainMatcher{}, fmt.Errorf("domain value has invalid wire type")
			}
			value, valueLen := protowire.ConsumeBytes(data)
			if valueLen < 0 {
				return domainMatcher{}, fmt.Errorf("invalid domain value: %v", protowire.ParseError(valueLen))
			}
			matcher.value = strings.ToLower(strings.TrimSuffix(string(value), "."))
			data = data[valueLen:]
		default:
			skip := protowire.ConsumeFieldValue(number, wireType, data)
			if skip < 0 {
				return domainMatcher{}, fmt.Errorf("invalid domain field: %v", protowire.ParseError(skip))
			}
			data = data[skip:]
		}
	}
	if matcher.kind == "regexp" {
		re, err := regexp.Compile(matcher.value)
		if err != nil {
			return domainMatcher{}, fmt.Errorf("compile GeoSite regexp %q: %w", matcher.value, err)
		}
		matcher.re = re
	}
	return matcher, nil
}
