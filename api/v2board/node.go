package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
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
	SpeedLimit          int              `json:"speed_limit"`
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
	Email            string   `json:"email"`
	Provider         string   `json:"provider"`
	DNSEnv           string   `json:"dns_env"`
	RejectUnknownSni string   `json:"reject_unknown_sni"`
	AllowInsecure    string   `json:"allow_insecure"`
	Fingerprint      string   `json:"fingerprint"`
	ECH              string   `json:"ech"`
	ECHServerName    string   `json:"ech_server_name"`
	ECHKey           string   `json:"ech_key"`
	ECHConfig        any      `json:"ech_config"`
}

type CertInfo struct {
	CertMode         string
	CertFile         string
	KeyFile          string
	Email            string
	CertDomain       string
	CertDomains      []string
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
	if common.SpeedLimit < 0 {
		return nil, fmt.Errorf("invalid node speed limit: %d Mbps", common.SpeedLimit)
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
		certMode := strings.ToLower(strings.TrimSpace(common.TlsSettings.CertMode))
		if certMode == "" {
			return nil, fmt.Errorf("NaiveProxy certificate mode is empty")
		}
		if !isSupportedNaiveCertMode(certMode) {
			return nil, fmt.Errorf("unsupported NaiveProxy certificate mode: %s", certMode)
		}
		if common.Tls != Tls {
			return nil, fmt.Errorf("NaiveProxy certificate mode %s requires TLS", certMode)
		}
		if common.TlsSettings.PrimaryServerName() == "" {
			return nil, fmt.Errorf("NaiveProxy TLS server name is empty")
		}
		if certMode == "none" && common.TransportProtocol != "TCP" {
			return nil, fmt.Errorf("NaiveProxy without a certificate only supports TCP relay nodes")
		}
		if common.UserNamePrefix == "" {
			common.UserNamePrefix = fmt.Sprintf("n%d", c.NodeId)
		}
	}
	if common.BaseConfig == nil {
		common.BaseConfig = &BaseConfig{PushInterval: 60, PullInterval: 60}
	}
	common.CertInfo, err = buildCertInfo(c.NodeId, common.Protocol, common.TlsSettings)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate settings: %w", err)
	}

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

func buildCertInfo(nodeID int, protocol string, settings TlsSettings) (*CertInfo, error) {
	certMode := strings.ToLower(strings.TrimSpace(settings.CertMode))
	certFile := strings.TrimSpace(settings.CertFile)
	if certFile == "" {
		certFile = filepath.Join("/etc/daonode", protocol+strconv.Itoa(nodeID)+".cer")
	}
	keyFile := strings.TrimSpace(settings.KeyFile)
	if keyFile == "" {
		keyFile = filepath.Join("/etc/daonode", protocol+strconv.Itoa(nodeID)+".key")
	}

	certDomains := settings.CertificateNames()
	certDomain := ""
	if len(certDomains) > 0 {
		certDomain = certDomains[0]
	}
	var (
		email  string
		dnsEnv map[string]string
		err    error
	)
	if certMode == "http" || certMode == "dns" {
		if len(certDomains) == 0 {
			return nil, fmt.Errorf("ACME certificate domain is empty")
		}
		for index, domain := range certDomains {
			certDomains[index], err = normalizeACMEDomain(domain)
			if err != nil {
				return nil, err
			}
		}
		certDomain = certDomains[0]
		email, err = normalizeACMEEmail(settings.Email, certDomain)
		if err != nil {
			return nil, err
		}
	}
	if certMode == "dns" {
		if strings.TrimSpace(settings.Provider) == "" {
			return nil, fmt.Errorf("DNS certificate mode requires a provider")
		}
		dnsEnv, err = parseDNSEnv(settings.DNSEnv)
		if err != nil {
			return nil, err
		}
		if len(dnsEnv) == 0 {
			return nil, fmt.Errorf("DNS certificate mode requires DNS environment variables")
		}
	}
	return &CertInfo{
		CertMode:         certMode,
		CertFile:         certFile,
		KeyFile:          keyFile,
		Email:            email,
		CertDomain:       certDomain,
		CertDomains:      certDomains,
		DNSEnv:           dnsEnv,
		Provider:         strings.TrimSpace(settings.Provider),
		RejectUnknownSni: settings.RejectUnknownSni == "1",
	}, nil
}

func (t TlsSettings) PrimaryServerName() string {
	if serverName := strings.TrimSpace(t.ServerName); serverName != "" {
		return serverName
	}
	for _, serverName := range t.ServerNames {
		if serverName = strings.TrimSpace(serverName); serverName != "" {
			return serverName
		}
	}
	return ""
}

func (t TlsSettings) CertificateNames() []string {
	values := make([]string, 0, len(t.ServerNames)+1)
	values = append(values, t.ServerName)
	values = append(values, t.ServerNames...)
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeACMEEmail(value, certDomain string) (string, error) {
	email := strings.TrimSpace(value)
	if email == "" {
		email = "acme@" + certDomain
	}
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(address.Address, email) {
		return "", fmt.Errorf("ACME email is invalid")
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at <= 0 || at == len(address.Address)-1 {
		return "", fmt.Errorf("ACME email is invalid")
	}
	domain, err := normalizeACMEDomain(address.Address[at+1:])
	if err != nil {
		return "", fmt.Errorf("ACME email domain: %w", err)
	}
	return strings.ToLower(address.Address[:at]) + "@" + domain, nil
}

func normalizeACMEDomain(value string) (string, error) {
	domain := strings.TrimSuffix(strings.TrimSpace(value), ".")
	if domain == "" || net.ParseIP(domain) != nil {
		return "", fmt.Errorf("ACME requires a public DNS name, not an IP address")
	}
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", fmt.Errorf("invalid ACME domain: %w", err)
	}
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if len(ascii) == 0 || len(ascii) > 253 {
		return "", fmt.Errorf("invalid ACME domain")
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid ACME domain")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("invalid ACME domain")
			}
		}
	}
	publicSuffix, isICANN := publicsuffix.PublicSuffix(ascii)
	// The public suffix list also contains explicitly registered private
	// suffixes such as pages.dev and github.io. They are valid ACME targets
	// when the concrete hostname points to this server, even though the PSL
	// marks the suffix as private instead of ICANN-managed. Unknown/special
	// use suffixes fall back to a single-label rule and remain rejected.
	recognizedPrivateSuffix := !isICANN && strings.Contains(publicSuffix, ".")
	if publicSuffix == ascii || (!isICANN && !recognizedPrivateSuffix) {
		return "", fmt.Errorf(
			"ACME domain %q must contain a registrable name under a recognized public suffix",
			ascii,
		)
	}
	return ascii, nil
}

func parseDNSEnv(value string) (map[string]string, error) {
	variables := make(map[string]string)
	for _, item := range strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == '\n' || char == '\r'
	}) {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) != 2 || !isEnvironmentVariableName(parts[0]) {
			return nil, fmt.Errorf("invalid DNS environment variable %q; use NAME=value", item)
		}
		variables[parts[0]] = parts[1]
	}
	return variables, nil
}

func isEnvironmentVariableName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_' {
			continue
		}
		if index > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func isSupportedNaiveCertMode(value string) bool {
	switch value {
	case "self", "http", "dns", "file", "none":
		return true
	default:
		return false
	}
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
