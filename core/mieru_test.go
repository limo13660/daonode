package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	mieruclient "github.com/enfein/mieru/v3/apis/client"
	"github.com/enfein/mieru/v3/apis/model"
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

func TestMieruRuntimeUDPStarts(t *testing.T) {
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate UDP port: %v", err)
	}
	proxyPort := packetConn.LocalAddr().(*net.UDPAddr).Port
	packetConn.Close()

	runtime := newMieruRuntime("integration-udp", &panel.NodeInfo{
		Id:   92,
		Type: "mieru",
		Common: &panel.CommonNode{
			ServerPort:        proxyPort,
			TransportProtocol: "UDP",
			MTU:               1400,
			UserNamePrefix:    "integrationudp",
		},
	})
	runtime.users["udp-password"] = panel.UserInfo{Id: 13, Uuid: "udp-password"}
	if err := runtime.Start(); err != nil {
		t.Fatalf("start UDP Mieru runtime: %v", err)
	}
	if err := runtime.Stop(); err != nil {
		t.Fatalf("stop UDP Mieru runtime: %v", err)
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
