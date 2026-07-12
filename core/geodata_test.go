package core

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestGeoDataStoreLoadsV2RayDatRecords(t *testing.T) {
	dir := t.TempDir()
	geoIPPath := filepath.Join(dir, "geoip.dat")
	geoSitePath := filepath.Join(dir, "geosite.dat")

	ip := net.ParseIP("203.0.113.0").To4()
	cidr := protowire.AppendTag(nil, 1, protowire.BytesType)
	cidr = protowire.AppendBytes(cidr, ip)
	cidr = protowire.AppendTag(cidr, 2, protowire.VarintType)
	cidr = protowire.AppendVarint(cidr, 24)
	geoIP := protowire.AppendTag(nil, 2, protowire.BytesType)
	geoIP = protowire.AppendBytes(geoIP, cidr)

	domain := protowire.AppendTag(nil, 1, protowire.VarintType)
	domain = protowire.AppendVarint(domain, 2) // Domain.Domain
	domain = protowire.AppendTag(domain, 2, protowire.BytesType)
	domain = protowire.AppendBytes(domain, []byte("example.com"))
	geoSite := protowire.AppendTag(nil, 2, protowire.BytesType)
	geoSite = protowire.AppendBytes(geoSite, domain)

	if err := os.WriteFile(geoIPPath, makeGeoDataRecord("TEST", geoIP), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(geoSitePath, makeGeoDataRecord("TEST", geoSite), 0600); err != nil {
		t.Fatal(err)
	}

	engine, err := newRouteEngineWithGeoData([]panel.Route{
		{Id: 1, Action: "block_ip", Match: []string{"geoip:test"}},
		{Id: 2, Action: "block", Match: []string{"geosite:test"}},
	}, newGeoDataStoreWithPaths(geoIPPath, geoSitePath))
	if err != nil {
		t.Fatalf("load v2ray geodata: %v", err)
	}
	if got := engine.decision(routeTarget{ip: net.ParseIP("203.0.113.42"), port: 443}); got != routeBlock {
		t.Fatalf("GeoIP route decision = %v, want block", got)
	}
	if got := engine.decision(routeTarget{host: "www.example.com", port: 443}); got != routeBlock {
		t.Fatalf("GeoSite route decision = %v, want block", got)
	}
}

func TestGeoDataStoreLoadsRealV2RayDatFiles(t *testing.T) {
	geoIPPath := os.Getenv("DAONODE_TEST_GEOIP_PATH")
	geoSitePath := os.Getenv("DAONODE_TEST_GEOSITE_PATH")
	if geoIPPath == "" || geoSitePath == "" {
		t.Skip("real v2ray dat paths are not configured")
	}
	store := newGeoDataStoreWithPaths(geoIPPath, geoSitePath)
	if prefixes, err := store.loadIP("CN"); err != nil || len(prefixes) == 0 {
		t.Fatalf("load real GeoIP CN: %v", err)
	}
	if domains, err := store.loadSite("CN"); err != nil || len(domains) == 0 {
		t.Fatalf("load real GeoSite CN: %v", err)
	}
}

func makeGeoDataRecord(code string, payload []byte) []byte {
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, []byte(code))
	body = append(body, payload...)
	record := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(record, body)
}
