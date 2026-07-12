package core

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/enfein/mieru/v3/apis/model"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestRouteEngineBlocksDomainIPPortAndProtocol(t *testing.T) {
	engine, err := newRouteEngine([]panel.Route{
		{Id: 1, Action: "block", Match: []string{"domain:example.com"}},
		{Id: 2, Action: "block_ip", Match: []string{"10.0.0.0/8"}},
		{Id: 3, Action: "block_port", Match: []string{"1000-2000", "5353"}},
		{Id: 4, Action: "protocol", Match: []string{"quic"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []routeTarget{
		{host: "api.example.com", port: 443, network: "tcp"},
		{ip: net.ParseIP("10.2.3.4"), port: 80, network: "tcp"},
		{ip: net.ParseIP("192.0.2.1"), port: 1500, network: "tcp"},
	}
	for _, target := range tests {
		if got := engine.decision(target); got != routeBlock {
			t.Fatalf("decision(%+v) = %v, want block", target, got)
		}
	}
	if got := engine.decision(routeTarget{host: "allowed.example", port: 443, network: "tcp"}); got != routeDirect {
		t.Fatalf("allowed target decision = %v", got)
	}
	if got := engine.decisionWithProtocol(routeTarget{ip: net.ParseIP("192.0.2.1"), port: 443, network: "udp"}, "quic"); got != routeBlock {
		t.Fatalf("QUIC protocol decision = %v, want block", got)
	}
}

func TestRouteEnginePreservesRuleOrder(t *testing.T) {
	direct := `{"protocol":"freedom","tag":"direct"}`
	engine, err := newRouteEngine([]panel.Route{
		{Id: 1, Action: "route", Match: []string{"domain:example.com"}, ActionValue: &direct},
		{Id: 2, Action: "block", Match: []string{"example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.decision(routeTarget{host: "api.example.com", port: 443, network: "tcp"}); got != routeDirect {
		t.Fatalf("ordered route decision = %v, want direct", got)
	}

	plainDirect := "direct"
	plainEngine, err := newRouteEngine([]panel.Route{{
		Id:          3,
		Action:      "default_out",
		ActionValue: &plainDirect,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plainEngine.decision(routeTarget{host: "plain.example", port: 443}); got != routeDirect {
		t.Fatalf("plain direct route decision = %v, want direct", got)
	}
}

func TestRouteEngineRejectsUnsupportedXrayOnlyRules(t *testing.T) {
	shadowsocks := `{"protocol":"shadowsocks","tag":"ss-out"}`
	for _, route := range []panel.Route{
		{Id: 1, Action: "block", Match: []string{"geosite:cn"}},
		{Id: 2, Action: "block_ip", Match: []string{"geoip:cn"}},
		{Id: 3, Action: "default_out", ActionValue: &shadowsocks},
	} {
		if _, err := newRouteEngine([]panel.Route{route}); err == nil {
			t.Fatalf("newRouteEngine(%+v) accepted unsupported rule", route)
		}
	}
}

func TestRouteEngineBlocksTCPDialBeforeConnecting(t *testing.T) {
	engine, err := newRouteEngine([]panel.Route{{Id: 1, Action: "block_port", Match: []string{"443"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.dialTCP(t.Context(), model.AddrSpec{IP: net.ParseIP("127.0.0.1"), Port: 443})
	if !errors.Is(err, errRouteBlocked) {
		t.Fatalf("dialTCP() error = %v, want route block", err)
	}
}

func TestNormalizeDNSServer(t *testing.T) {
	tests := map[string]string{
		"8.8.8.8":         "8.8.8.8:53",
		"1.1.1.1:5353":    "1.1.1.1:5353",
		"2001:4860::8888": "[2001:4860::8888]:53",
		"dns.example":     "dns.example:53",
	}
	for input, want := range tests {
		got, err := normalizeDNSServer(input)
		if err != nil {
			t.Fatalf("normalizeDNSServer(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeDNSServer(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRouteEngineSelectsDomainDNS(t *testing.T) {
	server := "8.8.8.8"
	engine, err := newRouteEngine([]panel.Route{{
		Id: 1, Action: "dns", Match: []string{"domain:example.com"}, ActionValue: &server,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if engine.resolverForHost("api.example.com") == net.DefaultResolver {
		t.Fatal("matching domain did not select the configured DNS server")
	}
	if engine.resolverForHost("example.net") != net.DefaultResolver {
		t.Fatal("unmatched domain did not use the default DNS resolver")
	}
}

func TestRouteEngineCachesUDPAddress(t *testing.T) {
	engine := &routeEngine{}
	now := time.Now()
	want := net.ParseIP("192.0.2.10")
	engine.storeUDPIP("Example.COM.", want, now)

	got, ok := engine.cachedUDPIP("example.com", now.Add(time.Minute))
	if !ok || !got.Equal(want) {
		t.Fatalf("cachedUDPIP() = %v, %v, want %v, true", got, ok, want)
	}
	got[0] ^= 0xff
	again, ok := engine.cachedUDPIP("example.com", now.Add(time.Minute))
	if !ok || !again.Equal(want) {
		t.Fatal("cached UDP IP was returned without a defensive copy")
	}
	if _, ok := engine.cachedUDPIP("example.com", now.Add(udpDNSCacheTTL)); ok {
		t.Fatal("expired UDP DNS cache entry was returned")
	}
}

func TestMieruUDPDatagramRoundTrip(t *testing.T) {
	want := model.AddrSpec{FQDN: "udp.example", Port: 5353}
	header, err := mieruUDPHeader(want)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("route-test")
	packet := append(append([]byte(nil), header...), payload...)
	got, parsedHeader, parsedPayload, err := parseMieruUDPDatagram(packet)
	if err != nil {
		t.Fatal(err)
	}
	if got.FQDN != want.FQDN || got.Port != want.Port || string(parsedPayload) != string(payload) {
		t.Fatalf("parsed datagram = %+v %q", got, parsedPayload)
	}
	if string(parsedHeader) != string(header) {
		t.Fatal("UDP header changed during parsing")
	}
}

func TestSniffProtocol(t *testing.T) {
	tests := []struct {
		network string
		payload []byte
		want    string
	}{
		{network: "tcp", payload: []byte("GET / HTTP/1.1\r\n"), want: "http"},
		{network: "tcp", payload: []byte{0x16, 0x03, 0x03, 0x00}, want: "tls"},
		{network: "tcp", payload: append([]byte{19}, []byte("BitTorrent protocol")...), want: "bittorrent"},
		{network: "udp", payload: []byte{0xc0, 0, 0, 0, 1}, want: "quic"},
	}
	for _, test := range tests {
		if got := sniffProtocol(test.network, test.payload); got != test.want {
			t.Fatalf("sniffProtocol(%s, %x) = %q, want %q", test.network, test.payload, got, test.want)
		}
	}
}
