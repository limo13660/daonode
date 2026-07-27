package juicity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/daeuniverse/outbound/pool"
	"github.com/daeuniverse/outbound/protocol"
	juicityProtocol "github.com/daeuniverse/outbound/protocol/juicity"
	"github.com/daeuniverse/outbound/protocol/trojanc"
	"github.com/daeuniverse/outbound/protocol/tuic"
	tuicCommon "github.com/daeuniverse/outbound/protocol/tuic/common"
	quic "github.com/daeuniverse/quic-go"
	"github.com/google/uuid"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/limiter"
)

func TestOfficialJuicityTCPUsesCommonAccounting(t *testing.T) {
	limiter.Init()
	port := reserveUDPPort(t)
	info := testNodeInfo(t, port)
	user := panel.UserInfo{Id: 27, Uuid: "2a506d42-cc67-4f92-9d85-303467108433", DeviceLimit: 1}
	tag := "juicity-" + t.Name()
	currentLimiter := limiter.AddLimiter("juicity", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	current := NewRuntime(tag, info).(*runtime)
	if _, err := current.AddUsers([]panel.UserInfo{user}); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	t.Cleanup(func() { _ = current.Stop() })

	target := startEchoServer(t)
	client, transport := dialTestClient(t, port, user.Uuid)
	t.Cleanup(func() {
		_ = client.CloseWithError(0, "")
		_ = transport.Close()
	})
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	targetPort, _ := strconv.Atoi(portText)
	stream, err := client.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatalf("open Juicity stream: %v", err)
	}
	proxy := juicityProtocol.NewConn(stream, &trojanc.Metadata{
		Metadata: protocol.Metadata{
			Type:     protocol.MetadataTypeIPv4,
			Hostname: host,
			Port:     uint16(targetPort),
			IsClient: true,
		},
		Network: "tcp",
	}, nil)
	t.Cleanup(func() { _ = proxy.Close() })

	payload := []byte("juicity-common-accounting")
	if _, err := proxy.Write(payload); err != nil {
		t.Fatalf("write Juicity payload: %v", err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(proxy, received); err != nil {
		t.Fatalf("read Juicity echo: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("echoed payload = %q, want %q", received, payload)
	}

	traffic, err := current.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	want := []panel.UserTraffic{{UID: user.Id, Upload: int64(len(payload)), Download: int64(len(payload))}}
	if !reflect.DeepEqual(traffic, want) {
		t.Fatalf("Traffic() = %#v, want %#v", traffic, want)
	}
	online, err := currentLimiter.GetOnlineDevice()
	if err != nil || len(*online) != 1 {
		t.Fatalf("online devices = %#v, err = %v", online, err)
	}

	if err := current.SyncUsers([]panel.UserInfo{user}, nil); err != nil {
		t.Fatalf("delete Juicity user: %v", err)
	}
	select {
	case <-client.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("deleted Juicity user's active QUIC connection remained open")
	}
}

func TestJuicityValidationAndHotCredentialSnapshot(t *testing.T) {
	info := testNodeInfo(t, reserveUDPPort(t))
	current := NewRuntime("juicity-validation", info).(*runtime)
	invalid := panel.UserInfo{Id: 1, Uuid: "not-a-uuid"}
	if err := current.Validate([]panel.UserInfo{invalid}); err == nil {
		t.Fatal("Validate() accepted an invalid Juicity UUID")
	}
	info.Common.TransportProtocol = "TCP"
	if err := current.Validate(nil); err == nil {
		t.Fatal("Validate() accepted a TCP Juicity listener")
	}
	info.Common.TransportProtocol = "UDP"
	info.Common.CertInfo.CertMode = "none"
	if err := current.Validate(nil); err == nil {
		t.Fatal("Validate() accepted Juicity without TLS certificate")
	}
}

func TestOfficialJuicityIgnoresUnsupportedSemanticRoutes(t *testing.T) {
	policy, err := compileRoutePolicy([]panel.Route{
		{Id: 10, Action: "dns"},
		{Id: 11, Action: "protocol", Match: []string{"bittorrent"}},
	})
	if err != nil {
		t.Fatalf("compileRoutePolicy() rejected ignorable routes: %v", err)
	}
	if policy.blockAll || len(policy.domains) != 0 || len(policy.prefixes) != 0 || len(policy.ports) != 0 {
		t.Fatalf("ignored route policy = %#v", policy)
	}
}

func TestOfficialJuicityUDPUsesCommonAccounting(t *testing.T) {
	limiter.Init()
	port := reserveUDPPort(t)
	info := testNodeInfo(t, port)
	user := panel.UserInfo{Id: 28, Uuid: "6ae981ad-e674-4e2d-bd9d-e21e5bfe8f52"}
	tag := "juicity-" + t.Name()
	limiter.AddLimiter("juicity", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	current := NewRuntime(tag, info).(*runtime)
	if _, err := current.AddUsers([]panel.UserInfo{user}); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	t.Cleanup(func() { _ = current.Stop() })

	target, closeEcho := startUDPEchoServer(t)
	defer closeEcho()
	client, transport := dialTestClient(t, port, user.Uuid)
	t.Cleanup(func() {
		_ = client.CloseWithError(0, "")
		_ = transport.Close()
	})
	host, portText, _ := net.SplitHostPort(target.String())
	targetPort, _ := strconv.Atoi(portText)
	stream, err := client.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatalf("open Juicity UDP stream: %v", err)
	}
	connection := juicityProtocol.NewConn(stream, &trojanc.Metadata{
		Metadata: protocol.Metadata{
			Type:     protocol.MetadataTypeIPv4,
			Hostname: host,
			Port:     uint16(targetPort),
			IsClient: true,
		},
		Network: "udp",
	}, nil)
	packet := &juicityProtocol.PacketConn{Conn: connection}
	t.Cleanup(func() { _ = packet.Close() })
	payload := []byte("juicity-udp-accounting")
	if _, err := packet.WriteTo(payload, target.String()); err != nil {
		t.Fatalf("write Juicity UDP payload: %v", err)
	}
	received := make([]byte, len(payload))
	n, _, err := packet.ReadFrom(received)
	if err != nil {
		t.Fatalf("read Juicity UDP echo: %v", err)
	}
	if !bytes.Equal(received[:n], payload) {
		t.Fatalf("UDP echo = %q, want %q", received[:n], payload)
	}
	traffic, err := current.Traffic(0)
	if err != nil {
		t.Fatal(err)
	}
	want := []panel.UserTraffic{{UID: user.Id, Upload: int64(len(payload)), Download: int64(len(payload))}}
	if !reflect.DeepEqual(traffic, want) {
		t.Fatalf("Traffic() = %#v, want %#v", traffic, want)
	}
}

func dialTestClient(t *testing.T, port int, credential string) (quic.Connection, *quic.Transport) {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for Juicity client: %v", err)
	}
	transport := &quic.Transport{Conn: packet}
	address := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, address, &tls.Config{
		ServerName:         "node.test",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h3"},
	}, &quic.Config{
		InitialStreamReceiveWindow:     tuicCommon.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         tuicCommon.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: tuicCommon.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     tuicCommon.MaxConnectionReceiveWindow,
		KeepAlivePeriod:                time.Second,
	})
	if err != nil {
		_ = transport.Close()
		t.Fatalf("dial Juicity runtime: %v", err)
	}
	id := uuid.MustParse(credential)
	stream, err := conn.OpenUniStream()
	if err != nil {
		t.Fatalf("open Juicity authentication stream: %v", err)
	}
	token, err := tuic.GenToken(conn.ConnectionState(), id, credential)
	if err != nil {
		t.Fatalf("generate Juicity token: %v", err)
	}
	buffer := pool.GetBuffer()
	defer pool.PutBuffer(buffer)
	if err := tuic.NewAuthenticate(id, token, juicityProtocol.Version0).WriteTo(buffer); err != nil {
		t.Fatalf("encode Juicity authentication: %v", err)
	}
	if _, err := buffer.WriteTo(stream); err != nil {
		t.Fatalf("write Juicity authentication: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close Juicity authentication stream: %v", err)
	}
	return conn, transport
}

func testNodeInfo(t *testing.T, port int) *panel.NodeInfo {
	t.Helper()
	certFile, keyFile := writeTestCertificate(t)
	return &panel.NodeInfo{
		Id:       1,
		Type:     "juicity",
		Kernel:   "juicity",
		Security: panel.Tls,
		Common: &panel.CommonNode{
			Protocol:          "juicity",
			Kernel:            "juicity",
			ListenIP:          "127.0.0.1",
			ServerPort:        port,
			TransportProtocol: "UDP",
			Tls:               panel.Tls,
			TlsSettings: panel.TlsSettings{
				ServerName: "node.test",
				CertMode:   "file",
			},
			CertInfo: &panel.CertInfo{
				CertMode:    "file",
				CertFile:    certFile,
				KeyFile:     keyFile,
				CertDomains: []string{"node.test"},
			},
			ProtocolSettings: panel.ProtocolSettings{QUICCongestionControl: "bbr"},
		},
	}
}

func writeTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "node.test"},
		DNSNames:     []string{"node.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certFile := filepath.Join(directory, "server.crt")
	keyFile := filepath.Join(directory, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func reserveUDPPort(t *testing.T) int {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := packet.LocalAddr().(*net.UDPAddr).Port
	if err := packet.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	return listener.Addr().String()
}

func startUDPEchoServer(t *testing.T) (*net.UDPAddr, func()) {
	t.Helper()
	packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, address, err := packet.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = packet.WriteToUDP(buffer[:n], address)
		}
	}()
	return packet.LocalAddr().(*net.UDPAddr), func() { _ = packet.Close() }
}

var _ = fmt.Sprintf
