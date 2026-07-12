package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Security     int
	PushInterval time.Duration
	PullInterval time.Duration
	Tag          string
	Common       *CommonNode
}

type CommonNode struct {
	Protocol          string      `json:"protocol"`
	ServerPort        int         `json:"server_port"`
	TransportProtocol string      `json:"transport_protocol"`
	MTU               int         `json:"mtu"`
	UserNamePrefix    string      `json:"username_prefix"`
	BaseConfig        *BaseConfig `json:"base_config"`
	Tls               int         `json:"tls"`
	TlsSettings       TlsSettings `json:"tls_settings"`
	CertInfo          *CertInfo   `json:"-"`
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
		return nil, err
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
	if common.Protocol != "mieru" {
		return nil, fmt.Errorf("unsupported protocol: %s", common.Protocol)
	}
	if common.ServerPort < 1 || common.ServerPort > 65535 {
		return nil, fmt.Errorf("invalid Mieru server port: %d", common.ServerPort)
	}
	if common.TransportProtocol == "" {
		common.TransportProtocol = "TCP"
	}
	if common.MTU == 0 {
		common.MTU = 1400
	}
	if common.UserNamePrefix == "" {
		common.UserNamePrefix = fmt.Sprintf("n%d", c.NodeId)
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
		Type:         "mieru",
		Security:     common.Tls,
		PushInterval: pushInterval,
		PullInterval: pullInterval,
		Tag:          fmt.Sprintf("[%s]-mieru:%d", c.APIHost, c.NodeId),
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
	switch v := value.(type) {
	case nil:
		return time.Minute, nil
	case int:
		return time.Duration(v) * time.Second, nil
	case int64:
		return time.Duration(v) * time.Second, nil
	case float64:
		return time.Duration(v) * time.Second, nil
	case string:
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return 0, err
		}
		return time.Duration(seconds) * time.Second, nil
	default:
		return 0, fmt.Errorf("unsupported value type %T", value)
	}
}
