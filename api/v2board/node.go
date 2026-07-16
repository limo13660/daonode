package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	None = iota
	Tls
	Reality
)

type NodeInfo struct {
	Id           int
	Type         string
	Kernel       string
	Security     int
	PushInterval time.Duration
	PullInterval time.Duration
	Tag          string
	Common       *CommonNode
}

type CommonNode struct {
	Protocol            string           `json:"protocol"`
	Kernel              string           `json:"kernel"`
	ListenIP            string           `json:"listen_ip"`
	ServerPort          int              `json:"server_port"`
	TransportProtocol   string           `json:"transport_protocol"`
	PortBindings        []PortBinding    `json:"port_bindings"`
	MTU                 int              `json:"mtu"`
	TrafficPattern      string           `json:"traffic_pattern"`
	UserHintIsMandatory bool             `json:"user_hint_is_mandatory"`
	UserNamePrefix      string           `json:"username_prefix"`
	ProtocolSettings    ProtocolSettings `json:"protocol_settings"`
	Routes              []Route          `json:"routes"`
	BaseConfig          *BaseConfig      `json:"base_config"`
	Tls                 int              `json:"tls"`
	TlsSettings         TlsSettings      `json:"tls_settings"`
	CertInfo            *CertInfo        `json:"-"`
}

type ProtocolSettings struct {
	QUICCongestionControl string `json:"quic_congestion_control"`
	UDPOverTCP            bool   `json:"udp_over_tcp"`
}

type PortBinding struct {
	Port       string `json:"port"`
	ServerPort string `json:"server_port"`
	Protocol   string `json:"protocol"`
}

type Route struct {
	Id          int      `json:"id"`
	Match       []string `json:"match"`
	Action      string   `json:"action"`
	ActionValue *string  `json:"action_value"`
}

type TlsSettings struct {
	ServerName       string   `json:"server_name"`
	ServerNames      []string `json:"server_names"`
	CertMode         string   `json:"cert_mode"`
	CertFile         string   `json:"cert_file"`
	KeyFile          string   `json:"key_file"`
	Provider         string   `json:"provider"`
	DNSEnv           string   `json:"dns_env"`
	RejectUnknownSni string   `json:"reject_unknown_sni"`
}

type CertInfo struct {
	CertMode         string
	CertFile         string
	KeyFile          string
	Email            string
	CertDomain       string
	DNSEnv           map[string]string
	Provider         string
	RejectUnknownSni bool
}

type BaseConfig struct {
	PushInterval           any `json:"push_interval"`
	PullInterval           any `json:"pull_interval"`
	DeviceOnlineMinTraffic int `json:"device_online_min_traffic"`
	NodeReportMinTraffic   int `json:"node_report_min_traffic"`
}

func (c *Client) GetNodeInfo(ctx context.Context) (*NodeInfo, error) {
	const path = "/api/v2/server/config"
	response, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.nodeEtag).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, c.requestError(err)
	}
	if response == nil {
		return nil, fmt.Errorf("received nil response")
	}
	if response.StatusCode() == 304 {
		return nil, nil
	}
	if response.IsError() {
		return nil, fmt.Errorf("get node config failed with status %d", response.StatusCode())
	}

	hash := sha256.Sum256(response.Body())
	newBodyHash := hex.EncodeToString(hash[:])
	if c.responseBodyHash == newBodyHash {
		return nil, nil
	}

	common := &CommonNode{}
	if err := json.Unmarshal(response.Body(), common); err != nil {
		return nil, fmt.Errorf("decode node params: %w", err)
	}
	common.Protocol = strings.ToLower(strings.TrimSpace(common.Protocol))
	if common.Protocol == "" {
		return nil, fmt.Errorf("node protocol is empty")
	}
	common.Kernel = strings.ToLower(strings.TrimSpace(common.Kernel))
	if common.Kernel == "" {
		return nil, fmt.Errorf("node kernel is empty for protocol %s", common.Protocol)
	}
	if common.ServerPort < 1 || common.ServerPort > 65535 {
		return nil, fmt.Errorf("invalid server port: %d", common.ServerPort)
	}
	common.ListenIP = strings.TrimSpace(common.ListenIP)
	if common.ListenIP == "" {
		common.ListenIP = "0.0.0.0"
	}
	if net.ParseIP(common.ListenIP) == nil {
		return nil, fmt.Errorf("invalid listen IP: %s", common.ListenIP)
	}
	if common.Protocol == "mieru" {
		if common.TransportProtocol == "" {
			common.TransportProtocol = "TCP"
		}
		common.TransportProtocol = strings.ToUpper(common.TransportProtocol)
		if common.TransportProtocol != "TCP" && common.TransportProtocol != "UDP" {
			return nil, fmt.Errorf("invalid Mieru transport protocol: %s", common.TransportProtocol)
		}
		for i := range common.PortBindings {
			binding := &common.PortBindings[i]
			binding.Port = strings.TrimSpace(binding.Port)
			binding.ServerPort = strings.TrimSpace(binding.ServerPort)
			binding.Protocol = strings.ToUpper(strings.TrimSpace(binding.Protocol))
			if binding.ServerPort == "" {
				binding.ServerPort = binding.Port
			}
			if binding.Port == "" || binding.ServerPort == "" {
				return nil, fmt.Errorf("Mieru port binding %d is empty", i)
			}
			if binding.Protocol != "TCP" && binding.Protocol != "UDP" {
				return nil, fmt.Errorf("invalid Mieru port binding protocol: %s", binding.Protocol)
			}
		}
		if common.MTU == 0 {
			common.MTU = 1400
		}
		if common.UserNamePrefix == "" {
			common.UserNamePrefix = fmt.Sprintf("n%d", c.NodeId)
		}
	}
	if common.Protocol == "naive" {
		if common.TransportProtocol == "" {
			common.TransportProtocol = "TCP"
		}
		common.TransportProtocol = strings.ToUpper(common.TransportProtocol)
		if common.TransportProtocol != "TCP" && common.TransportProtocol != "UDP" {
			return nil, fmt.Errorf("invalid NaiveProxy transport protocol: %s", common.TransportProtocol)
		}
		if common.Tls != Tls {
			return nil, fmt.Errorf("NaiveProxy requires TLS")
		}
		if common.TlsSettings.PrimaryServerName() == "" {
			return nil, fmt.Errorf("NaiveProxy TLS server name is empty")
		}
		if common.TlsSettings.CertMode == "" || common.TlsSettings.CertMode == "none" {
			return nil, fmt.Errorf("NaiveProxy certificate mode is empty")
		}
		if common.UserNamePrefix == "" {
			common.UserNamePrefix = fmt.Sprintf("n%d", c.NodeId)
		}
	}
	if common.BaseConfig == nil {
		common.BaseConfig = &BaseConfig{PushInterval: 60, PullInterval: 60}
	}
	common.CertInfo = buildCertInfo(c.NodeId, common.Protocol, common.TlsSettings)

	pushInterval, err := intervalToTime(common.BaseConfig.PushInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid push interval: %w", err)
	}
	pullInterval, err := intervalToTime(common.BaseConfig.PullInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid pull interval: %w", err)
	}

	c.responseBodyHash = newBodyHash
	c.nodeEtag = response.Header().Get("ETag")

	return &NodeInfo{
		Id:           c.NodeId,
		Type:         common.Protocol,
		Kernel:       common.Kernel,
		Security:     common.Tls,
		PushInterval: pushInterval,
		PullInterval: pullInterval,
		Tag:          fmt.Sprintf("[%s]-%s:%d", c.APIHost, common.Protocol, c.NodeId),
		Common:       common,
	}, nil
}

func buildCertInfo(nodeID int, protocol string, settings TlsSettings) *CertInfo {
	certFile := settings.CertFile
	if certFile == "" {
		certFile = filepath.Join("/etc/daonode", protocol+strconv.Itoa(nodeID)+".cer")
	}
	keyFile := settings.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join("/etc/daonode", protocol+strconv.Itoa(nodeID)+".key")
	}
	dnsEnv := make(map[string]string)
	for _, item := range strings.Split(settings.DNSEnv, ",") {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			dnsEnv[parts[0]] = parts[1]
		}
	}
	return &CertInfo{
		CertMode:         settings.CertMode,
		CertFile:         certFile,
		KeyFile:          keyFile,
		Email:            "node@daonode.local",
		CertDomain:       settings.PrimaryServerName(),
		DNSEnv:           dnsEnv,
		Provider:         settings.Provider,
		RejectUnknownSni: settings.RejectUnknownSni == "1",
	}
}

func (t TlsSettings) PrimaryServerName() string {
	if len(t.ServerNames) > 0 {
		return t.ServerNames[0]
	}
	return t.ServerName
}

func intervalToTime(value any) (time.Duration, error) {
	const (
		minimumIntervalSeconds int64 = 5
		maximumIntervalSeconds int64 = 24 * 60 * 60
	)
	var seconds int64
	switch v := value.(type) {
	case nil:
		return time.Minute, nil
	case int:
		seconds = int64(v)
	case int64:
		seconds = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < math.MinInt64 || v > math.MaxInt64 {
			return 0, fmt.Errorf("interval must be a whole number of seconds")
		}
		seconds = int64(v)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, err
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("unsupported value type %T", value)
	}
	if seconds < minimumIntervalSeconds || seconds > maximumIntervalSeconds {
		return 0, fmt.Errorf("interval %d seconds is outside %d-%d seconds", seconds, minimumIntervalSeconds, maximumIntervalSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
