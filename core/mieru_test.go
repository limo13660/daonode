package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	mieruclient "github.com/enfein/mieru/v3/apis/client"
	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/enfein/mieru/v3/apis/trafficpattern"
	"github.com/enfein/mieru/v3/pkg/appctl"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/enfein/mieru/v3/pkg/metrics"
	"google.golang.org/protobuf/proto"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestMieruTrafficCommit(t *testing.T) {
	runtime := newMieruRuntime("test", &panel.NodeInfo{
		Common: &panel.CommonNode{UserNamePrefix: "testnode"},
	})
	user := panel.UserInfo{Id: 7, Uuid: "test-password"}
	runtime.users[user.Uuid] = user
	userName := runtime.userName(user.Id)
	committedTraffic.Delete(userName)
	t.Cleanup(func() { committedTraffic.Delete(userName) })

	group := fmt.Sprintf(metrics.UserMetricGroupFormat, userName)
	metrics.RegisterMetric(group, metrics.UserMetricUploadBytes, metrics.COUNTER_TIME_SERIES).Add(120)
	metrics.RegisterMetric(group, metrics.UserMetricDownloadBytes, metrics.COUNTER_TIME_SERIES).Add(340)

	traffic, err := runtime.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	if len(traffic) != 1 || traffic[0].Upload != 120 || traffic[0].Download != 340 {
		t.Fatalf("Traffic() = %+v", traffic)
	}

	// An uncommitted snapshot must be returned again so a failed panel request
	// does not lose traffic.
	retry, _ := runtime.Traffic(0)
	if len(retry) != 1 || retry[0] != traffic[0] {
		t.Fatalf("retry Traffic() = %+v, want %+v", retry, traffic)
	}

	runtime.CommitTraffic(traffic)
	afterCommit, _ := runtime.Traffic(0)
	if len(afterCommit) != 0 {
		t.Fatalf("Traffic() after commit = %+v, want empty", afterCommit)
	}
}

func TestMieruTransport(t *testing.T) {
	if _, err := mieruTransport("TCP"); err != nil {
		t.Fatalf("TCP transport rejected: %v", err)
	}
	if _, err := mieruTransport("UDP"); err != nil {
		t.Fatalf("UDP transport rejected: %v", err)
	}
	if _, err := mieruTransport("QUIC"); err == nil {
		t.Fatal("unsupported transport was accepted")
	}
}

func TestCanonicalUDPAddrPort(t *testing.T) {
	mapped := netip.MustParseAddrPort("[::ffff:192.0.2.1]:53")
	want := netip.MustParseAddrPort("192.0.2.1:53")
	if got := canonicalUDPAddrPort(mapped); got != want {
		t.Fatalf("canonicalUDPAddrPort() = %v, want %v", got, want)
	}
}

func TestMieruOfficialPortBindings(t *testing.T) {
	bindings, err := mieruPortBindings(&panel.CommonNode{
		ServerPort:        443,
		TransportProtocol: "TCP",
		PortBindings: []panel.PortBinding{
			{ServerPort: "8443", Protocol: "UDP"},
			{ServerPort: "10000-10010", Protocol: "UDP"},
			{ServerPort: "443", Protocol: "TCP"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 3 {
		t.Fatalf("mieruPortBindings() returned %d bindings, want 3", len(bindings))
	}
	if bindings[0].GetPort() != 443 || bindings[0].GetProtocol() != appctlpb.TransportProtocol_TCP {
		t.Fatalf("primary binding = %+v", bindings[0])
	}
	if bindings[1].GetPort() != 8443 || bindings[1].GetProtocol() != appctlpb.TransportProtocol_UDP {
		t.Fatalf("UDP binding = %+v", bindings[1])
	}
	if bindings[2].GetPortRange() != "10000-10010" {
		t.Fatalf("range binding = %+v", bindings[2])
	}
	if !mieruHasUDPBinding(bindings) {
		t.Fatal("UDP binding was not detected")
	}
}

func TestMieruOfficialTrafficPattern(t *testing.T) {
	want := &appctlpb.TrafficPattern{Seed: proto.Int32(42), UnlockAll: proto.Bool(true)}
	data, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMieruTrafficPattern(base64.StdEncoding.EncodeToString(data))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("decodeMieruTrafficPattern() = %v, want %v", got, want)
	}
	if _, err := decodeMieruTrafficPattern("not-base64"); err == nil {
		t.Fatal("invalid traffic pattern was accepted")
	}
}

func TestMieruFullTrafficPatternRoundTrip(t *testing.T) {
	original := &appctlpb.TrafficPattern{
		Seed:      proto.Int32(12345),
		UnlockAll: proto.Bool(true),
		TcpFragment: &appctlpb.TCPFragment{
			Enable:     proto.Bool(true),
			MaxSleepMs: proto.Int32(50),
		},
		Nonce: &appctlpb.NoncePattern{
			Type:                appctlpb.NonceType_NONCE_TYPE_PRINTABLE.Enum(),
			ApplyToAllUDPPacket: proto.Bool(true),
			MinLen:              proto.Int32(4),
			MaxLen:              proto.Int32(8),
		},
		Padding: &appctlpb.PaddingPattern{
			MaxMiddlePaddingLen: proto.Int32(127),
			MaxEndPaddingLen:    proto.Int32(255),
		},
	}
	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMieruTrafficPattern(base64.StdEncoding.EncodeToString(data))
	if err != nil {
		t.Fatalf("decode full official traffic pattern: %v", err)
	}
	if !proto.Equal(original, decoded) {
		t.Fatalf("decoded traffic pattern = %v, want %v", decoded, original)
	}
	if _, err := trafficpattern.NewConfig(decoded); err != nil {
		t.Fatalf("official traffic pattern config: %v", err)
	}
}

func TestMieruOfficialShareURLCompatibility(t *testing.T) {
	pattern := &appctlpb.TrafficPattern{Seed: proto.Int32(42)}
	data, err := proto.Marshal(pattern)
	if err != nil {
		t.Fatal(err)
	}
	shareURL := "mierus://user:password@127.0.0.1?profile=official" +
		"&port=443&protocol=TCP" +
		"&port=8443&protocol=UDP" +
		"&port=10000-10010&protocol=UDP" +
		"&mtu=1280" +
		"&multiplexing=MULTIPLEXING_LOW" +
		"&handshake-mode=HANDSHAKE_NO_WAIT" +
		"&traffic-pattern=" + url.QueryEscape(base64.StdEncoding.EncodeToString(data))
	profile, err := appctl.URLToClientProfile(shareURL)
	if err != nil {
		t.Fatalf("official Mieru parser rejected share URL: %v", err)
	}
	if len(profile.GetServers()) != 1 || len(profile.GetServers()[0].GetPortBindings()) != 3 {
		t.Fatalf("official profile port bindings = %+v", profile.GetServers())
	}
	if profile.GetMtu() != 1280 || profile.GetHandshakeMode() != appctlpb.HandshakeMode_HANDSHAKE_NO_WAIT {
		t.Fatalf("official profile options = %+v", profile)
	}
	if !proto.Equal(profile.GetTrafficPattern(), pattern) {
		t.Fatalf("official profile traffic pattern = %v", profile.GetTrafficPattern())
	}
}

func TestMieruOfficialTrafficPatternValidation(t *testing.T) {
	invalid := &appctlpb.TrafficPattern{
		TcpFragment: &appctlpb.TCPFragment{MaxSleepMs: proto.Int32(101)},
	}
	data, err := proto.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeMieruTrafficPattern(base64.StdEncoding.EncodeToString(data)); err == nil {
		t.Fatal("officially invalid traffic pattern was accepted")
	}
}

func TestMieruGeneratedSixteenCharacterTrafficPattern(t *testing.T) {
	encoded := "CKPSvyIqBAgAEAA="
	if len(encoded) != 16 {
		t.Fatalf("generated traffic pattern length = %d, want 16", len(encoded))
	}
	pattern, err := trafficpattern.Decode(encoded)
	if err != nil {
		t.Fatalf("generated traffic pattern decode failed: %v", err)
	}
	if err := trafficpattern.Validate(pattern); err != nil {
		t.Fatalf("generated traffic pattern validation failed: %v", err)
	}
}

func TestBufferedPacketListener(t *testing.T) {
	listener := newBufferedPacketListener()
	conn, err := listener.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket() error = %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*net.UDPConn); !ok {
		t.Fatalf("ListenPacket() returned %T, want *net.UDPConn", conn)
	}
}

func TestMieruRuntimeTCP(t *testing.T) {
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo listener: %v", err)
	}
	defer echoListener.Close()
	go func() {
		conn, acceptErr := echoListener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	proxyPort := freeTCPPort(t)
	info := &panel.NodeInfo{
		Id:   91,
		Type: "mieru",
		Common: &panel.CommonNode{
			ServerPort:        proxyPort,
			TransportProtocol: "TCP",
			MTU:               1400,
			UserNamePrefix:    "integration",
			Routes:            []panel.Route{{Id: 1, Action: "block_port", Match: []string{"1"}}},
		},
	}
	runtime := newMieruRuntime("integration", info)
	user := panel.UserInfo{Id: 12, Uuid: "integration-password"}
	runtime.users[user.Uuid] = user
	if err := runtime.Start(); err != nil {
		t.Fatalf("start Mieru runtime: %v", err)
	}
	defer runtime.Stop()

	transport := appctlpb.TransportProtocol_TCP
	handshake := appctlpb.HandshakeMode_HANDSHAKE_STANDARD
	client := mieruclient.NewClient()
	if err := client.Store(&mieruclient.ClientConfig{
		Profile: &appctlpb.ClientProfile{
			ProfileName: proto.String("integration"),
			User: &appctlpb.User{
				Name:     proto.String(runtime.userName(user.Id)),
				Password: proto.String(user.Uuid),
			},
			Servers: []*appctlpb.ServerEndpoint{{
				IpAddress: proto.String("127.0.0.1"),
				PortBindings: []*appctlpb.PortBinding{{
					Port:     proto.Int32(int32(proxyPort)),
					Protocol: &transport,
				}},
			}},
			Mtu:           proto.Int32(1400),
			HandshakeMode: &handshake,
		},
	}); err != nil {
		t.Fatalf("store Mieru client config: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start Mieru client: %v", err)
	}
	defer client.Stop()

	target := echoListener.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, model.NetAddrSpec{
		AddrSpec: model.AddrSpec{IP: target.IP, Port: target.Port},
		Net:      "tcp",
	})
	if err != nil {
		t.Fatalf("dial through Mieru: %v", err)
	}
	defer conn.Close()

	payload := []byte("daonode-mieru")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through Mieru: %v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatalf("read through Mieru: %v", err)
	}
	if string(received) != string(payload) {
		t.Fatalf("received %q, want %q", received, payload)
	}
}

func TestMieruRuntimeUDP(t *testing.T) {
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("start UDP echo listener: %v", err)
	}
	defer echoConn.Close()
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, addr, readErr := echoConn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			_, _ = echoConn.WriteToUDP(buffer[:n], addr)
		}
	}()

	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate UDP port: %v", err)
	}
	proxyPort := packetConn.LocalAddr().(*net.UDPAddr).Port
	packetConn.Close()
	trafficPattern := &appctlpb.TrafficPattern{Seed: proto.Int32(42)}
	trafficPatternData, err := proto.Marshal(trafficPattern)
	if err != nil {
		t.Fatal(err)
	}

	info := &panel.NodeInfo{
		Id:   92,
		Type: "mieru",
		Common: &panel.CommonNode{
			ServerPort:          freeTCPPort(t),
			TransportProtocol:   "TCP",
			PortBindings:        []panel.PortBinding{{ServerPort: fmt.Sprint(proxyPort), Protocol: "UDP"}},
			MTU:                 1400,
			TrafficPattern:      base64.StdEncoding.EncodeToString(trafficPatternData),
			UserHintIsMandatory: true,
			UserNamePrefix:      "integrationudp",
			Routes:              []panel.Route{{Id: 1, Action: "block_port", Match: []string{"1"}}},
		},
	}
	runtime := newMieruRuntime("integration-udp", info)
	user := panel.UserInfo{Id: 13, Uuid: "udp-password"}
	runtime.users[user.Uuid] = user
	if err := runtime.Start(); err != nil {
		t.Fatalf("start UDP Mieru runtime: %v", err)
	}
	defer runtime.Stop()

	transport := appctlpb.TransportProtocol_UDP
	client := mieruclient.NewClient()
	if err := client.Store(&mieruclient.ClientConfig{Profile: &appctlpb.ClientProfile{
		ProfileName: proto.String("integration-udp"),
		User: &appctlpb.User{
			Name:     proto.String(runtime.userName(user.Id)),
			Password: proto.String(user.Uuid),
		},
		Servers: []*appctlpb.ServerEndpoint{{
			IpAddress: proto.String("127.0.0.1"),
			PortBindings: []*appctlpb.PortBinding{{
				Port:     proto.Int32(int32(proxyPort)),
				Protocol: &transport,
			}},
		}},
		Mtu:            proto.Int32(1400),
		TrafficPattern: trafficPattern,
	}}); err != nil {
		t.Fatalf("store UDP Mieru client config: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start UDP Mieru client: %v", err)
	}
	defer client.Stop()

	target := echoConn.LocalAddr().(*net.UDPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, model.NetAddrSpec{
		AddrSpec: model.AddrSpec{IP: target.IP, Port: target.Port},
		Net:      "udp",
	})
	if err != nil {
		t.Fatalf("dial UDP through Mieru: %v", err)
	}
	defer conn.Close()
	packetTunnel := apicommon.NewUDPAssociateWrapper(apicommon.NewPacketOverStreamTunnel(conn))
	defer packetTunnel.Close()
	_ = packetTunnel.SetDeadline(time.Now().Add(10 * time.Second))
	payload := []byte("daonode-mieru-udp")
	if _, err := packetTunnel.WriteTo(payload, target); err != nil {
		t.Fatalf("write UDP through Mieru: %v", err)
	}
	buffer := make([]byte, 2048)
	n, _, err := packetTunnel.ReadFrom(buffer)
	if err != nil {
		t.Fatalf("read UDP through Mieru: %v", err)
	}
	if string(buffer[:n]) != string(payload) {
		t.Fatalf("received UDP %q, want %q", buffer[:n], payload)
	}

	wantPackets := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		packet := fmt.Sprintf("mieru-udp-burst-%03d-%01024d", i, i)
		wantPackets[packet] = struct{}{}
		if _, err := packetTunnel.WriteTo([]byte(packet), target); err != nil {
			t.Fatalf("write UDP burst packet %d: %v", i, err)
		}
	}
	for i := 0; i < 64; i++ {
		n, _, err := packetTunnel.ReadFrom(buffer)
		if err != nil {
			t.Fatalf("read UDP burst packet %d: %v", i, err)
		}
		packet := string(buffer[:n])
		if _, ok := wantPackets[packet]; !ok {
			t.Fatalf("received unexpected UDP burst packet %q", packet)
		}
		delete(wantPackets, packet)
	}
	if len(wantPackets) != 0 {
		t.Fatalf("%d UDP burst packets were lost", len(wantPackets))
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
