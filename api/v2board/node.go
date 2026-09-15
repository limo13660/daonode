package panel

import (
	"bytes"
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
	Protocol            string        `json:"protocol"`
	Kernel              string        `json:"kernel"`
	ListenIP            string        `json:"listen_ip"`
	ServerPort          int           `json:"server_port"`
	SpeedLimit          int           `json:"speed_limit"`
	TransportProtocol   string        `json:"transport_protocol"`
	PortBindings        []PortBinding `json:"port_bindings"`
	MTU                 int           `json:"mtu"`
	TrafficPattern      string        `json:"traffic_pattern"`
	UserHintIsMandatory bool          `json:"user_hint_is_mandatory"`
	PanelIdentifier     string        `json:"panel_identifier"`
	// UserNamePrefix is the compatibility alias used by older DaoBoard builds.
	UserNamePrefix   string           `json:"username_prefix"`
	ProtocolSettings ProtocolSettings `json:"protocol_settings"`
	Routes           []Route          `json:"routes"`
	BaseConfig       *BaseConfig      `json:"base_config"`
	Tls              int              `json:"tls"`
	TlsSettings      TlsSettings      `json:"tls_settings"`
	CertInfo         *CertInfo        `json:"-"`
}

type ProtocolSettings struct {
	QUICCongestionControl string   `json:"quic_congestion_control"`
	UDPOverTCP            bool     `json:"udp_over_tcp"`
	AEADMethod            string   `json:"aead_method"`
	PaddingMin            int      `json:"padding_min"`
	PaddingMax            int      `json:"padding_max"`
	TableType             string   `json:"table_type"`
	EnablePureDownlink    bool     `json:"enable_pure_downlink"`
	HTTPMask              bool     `json:"http_mask"`
	HTTPMaskMode          string   `json:"http_mask_mode"`
	HTTPMaskTLS           bool     `json:"http_mask_tls"`
	HTTPMaskHost          string   `json:"http_mask_host"`
	PathRoot              string   `json:"path_root"`
	Multiplex             string   `json:"multiplex"`
	CustomTable           string   `json:"custom_table"`
	CustomTables          []string `json:"custom_tables"`
}

// UnmarshalJSON keeps the panel contract tolerant of older DaoBoard exports
// and of the names used by the official Sudoku/Mihomo configuration.  The
// panel now emits the flat canonical fields, but daonode may run against an
// older panel or a cached configuration while an installation is being
// upgraded.  Normalising at this boundary ensures every kernel sees one
// stable shape.
func (p *ProtocolSettings) UnmarshalJSON(data []byte) error {
	type plain ProtocolSettings
	trimmed := bytes.TrimSpace(data)
	// PHP's historical empty-array encoding (`[]`) is equivalent to an empty
	// settings object for this field.  DaoBoard emits `{}` today, but accepting
	// both avoids a needless node startup failure during upgrades.
	if bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		trimmed = []byte("{}")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return err
	}

	setAlias := func(canonical string, aliases ...string) {
		if value, exists := fields[canonical]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return
		}
		for _, alias := range aliases {
			if value, exists := fields[alias]; exists {
				fields[canonical] = value
				return
			}
		}
	}
	setAlias("aead_method", "aead")
	setAlias("table_type", "ascii")

	// Older clients represented the HTTP camouflage settings as a nested
	// object.  Accept both `disable` and `enabled`, while retaining precedence
	// for an explicitly supplied canonical field.
	if nested, ok := fields["httpmask"]; ok {
		var httpMask map[string]json.RawMessage
		if err := json.Unmarshal(nested, &httpMask); err == nil {
			if value, exists := fields["http_mask"]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				if value, exists := httpMask["enabled"]; exists {
					fields["http_mask"] = value
				} else if value, exists := httpMask["disable"]; exists {
					if disabled, ok := decodeJSONBool(value); ok {
						encoded, _ := json.Marshal(!disabled)
						fields["http_mask"] = encoded
					}
				}
			}
			for _, mapping := range [][2]string{
				{"mode", "http_mask_mode"}, {"tls", "http_mask_tls"}, {"host", "http_mask_host"},
				{"path_root", "path_root"}, {"path", "path_root"}, {"multiplex", "multiplex"},
			} {
				from, to := mapping[0], mapping[1]
				if value, exists := fields[to]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					continue
				}
				if value, exists := httpMask[from]; exists {
					fields[to] = value
				}
			}
		}
	}

	// custom_tables was briefly serialised as a JSON string by older admin
	// bundles.  Decode it when possible and silently use an empty list for a
	// malformed legacy value, matching DaoBoard's canonicalisation behaviour.
	if value, ok := fields["custom_tables"]; ok {
		var encoded string
		if json.Unmarshal(value, &encoded) == nil {
			var tables []string
			if json.Unmarshal([]byte(encoded), &tables) == nil {
				normalized, _ := json.Marshal(tables)
				fields["custom_tables"] = normalized
			} else {
				fields["custom_tables"] = json.RawMessage("[]")
			}
		}
	}
	for _, key := range []string{"udp_over_tcp", "enable_pure_downlink", "http_mask", "http_mask_tls"} {
		if value, ok := fields[key]; ok {
			if normalized, ok := decodeJSONBool(value); ok {
				encoded, _ := json.Marshal(normalized)
				fields[key] = encoded
			}
		}
	}
	for _, key := range []string{"padding_min", "padding_max"} {
		if value, ok := fields[key]; ok {
			var encoded string
			if json.Unmarshal(value, &encoded) == nil {
				if strings.TrimSpace(encoded) == "" {
					fields[key] = json.RawMessage("null")
					continue
				}
				number, err := strconv.Atoi(strings.TrimSpace(encoded))
				if err != nil {
					return fmt.Errorf("invalid Sudoku %s: %q", key, encoded)
				}
				fields[key] = json.RawMessage(strconv.Itoa(number))
			}
		}
	}

	// Apply the same defaults as DaoBoard when a legacy response omits the
	// Sudoku block entirely or only includes a subset of fields.  Explicit
	// zero/false values remain untouched because presence is checked above.
	defaults := map[string]string{
		"aead_method":          "\"chacha20-poly1305\"",
		"padding_min":          "10",
		"padding_max":          "30",
		"table_type":           "\"prefer_ascii\"",
		"enable_pure_downlink": "true",
		"http_mask":            "true",
		"http_mask_mode":       "\"legacy\"",
		"http_mask_tls":        "false",
		"multiplex":            "\"off\"",
		"custom_tables":        "[]",
	}
	for key, defaultValue := range defaults {
		if rawValue, exists := fields[key]; !exists || bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			fields[key] = json.RawMessage(defaultValue)
		}
	}

	normalized, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var decoded plain
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	decoded.AEADMethod = strings.ToLower(strings.TrimSpace(decoded.AEADMethod))
	decoded.TableType = strings.ToLower(strings.TrimSpace(decoded.TableType))
	if decoded.TableType == "prefer_numeric" {
		decoded.TableType = "prefer_ascii"
	} else if decoded.TableType == "custom" {
		decoded.TableType = "prefer_entropy"
	}
	decoded.HTTPMaskMode = strings.ToLower(strings.TrimSpace(decoded.HTTPMaskMode))
	if decoded.HTTPMaskMode == "split-stream" {
		decoded.HTTPMaskMode = "stream"
	}
	decoded.Multiplex = strings.ToLower(strings.TrimSpace(decoded.Multiplex))
	if decoded.Multiplex == "low" {
		decoded.Multiplex = "auto"
	} else if decoded.Multiplex == "high" {
		decoded.Multiplex = "on"
	}
	decoded.HTTPMaskHost = strings.TrimSpace(decoded.HTTPMaskHost)
	decoded.PathRoot = strings.Trim(decoded.PathRoot, " /\t\n\r\x00\v")
	decoded.CustomTable = strings.TrimSpace(decoded.CustomTable)
	cleanTables := decoded.CustomTables[:0]
	for _, table := range decoded.CustomTables {
		if table = strings.TrimSpace(table); table != "" {
			cleanTables = append(cleanTables, table)
		}
	}
	decoded.CustomTables = cleanTables
	*p = ProtocolSettings(decoded)
	return nil
}

func decodeJSONBool(value json.RawMessage) (bool, bool) {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return false, false
	}
	var b bool
	if json.Unmarshal(value, &b) == nil {
		return b, true
	}
	var n float64
	if json.Unmarshal(value, &n) == nil {
		return n != 0, true
	}
	var s string
	if json.Unmarshal(value, &s) != nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes", "y":
		return true, true
	case "0", "false", "off", "no", "n", "":
		return false, true
	default:
		return false, false
	}
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
	// A missing protocol_settings object does not invoke
	// ProtocolSettings.UnmarshalJSON.  Detect that legacy shape here so
	// Sudoku still receives the same defaults as DaoBoard.
	var envelope map[string]json.RawMessage
	settingsPresent := false
	if err := json.Unmarshal(response.Body(), &envelope); err == nil {
		if raw, ok := envelope["protocol_settings"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			settingsPresent = true
		}
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
	common.PanelIdentifier = strings.TrimSpace(common.PanelIdentifier)
	common.UserNamePrefix = strings.TrimSpace(common.UserNamePrefix)
	if common.PanelIdentifier != "" {
		common.UserNamePrefix = common.PanelIdentifier
	} else if common.UserNamePrefix == "" {
		common.UserNamePrefix = fmt.Sprintf("n%d", c.NodeId)
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
		if !isSupportedTLSCertMode(certMode) {
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
	}
	if common.Protocol == "juicity" {
		if common.TransportProtocol == "" {
			common.TransportProtocol = "UDP"
		}
		common.TransportProtocol = strings.ToUpper(common.TransportProtocol)
		if common.TransportProtocol != "UDP" {
			return nil, fmt.Errorf("Juicity transport protocol must be UDP")
		}
		certMode := strings.ToLower(strings.TrimSpace(common.TlsSettings.CertMode))
		if certMode == "" {
			return nil, fmt.Errorf("Juicity certificate mode is empty")
		}
		if !isSupportedTLSCertMode(certMode) || certMode == "none" {
			return nil, fmt.Errorf("unsupported Juicity certificate mode: %s", certMode)
		}
		if common.Tls != Tls {
			return nil, fmt.Errorf("Juicity requires TLS")
		}
		if common.TlsSettings.PrimaryServerName() == "" {
			return nil, fmt.Errorf("Juicity TLS server name is empty")
		}
		congestion := strings.ToLower(strings.TrimSpace(common.ProtocolSettings.QUICCongestionControl))
		if congestion == "" {
			congestion = "bbr"
		}
		switch congestion {
		case "bbr", "cubic", "new_reno":
			common.ProtocolSettings.QUICCongestionControl = congestion
		default:
			return nil, fmt.Errorf("unsupported Juicity congestion control: %s", congestion)
		}
	}
	if common.Protocol == "sudoku" {
		if !settingsPresent {
			common.ProtocolSettings = ProtocolSettings{
				AEADMethod:         "chacha20-poly1305",
				PaddingMin:         10,
				PaddingMax:         30,
				TableType:          "prefer_ascii",
				EnablePureDownlink: true,
				HTTPMask:           true,
				HTTPMaskMode:       "legacy",
				HTTPMaskTLS:        false,
				Multiplex:          "off",
				CustomTables:       []string{},
			}
		}
		if common.TransportProtocol == "" {
			common.TransportProtocol = "TCP"
		}
		common.TransportProtocol = strings.ToUpper(common.TransportProtocol)
		if common.TransportProtocol != "TCP" {
			return nil, fmt.Errorf("Sudoku transport protocol must be TCP")
		}
		if common.ProtocolSettings.AEADMethod == "" {
			common.ProtocolSettings.AEADMethod = "chacha20-poly1305"
		}
		if strings.TrimSpace(common.ProtocolSettings.TableType) == "" {
			common.ProtocolSettings.TableType = "prefer_ascii"
		}
		if strings.TrimSpace(common.ProtocolSettings.HTTPMaskMode) == "" {
			common.ProtocolSettings.HTTPMaskMode = "legacy"
		}
		if strings.TrimSpace(common.ProtocolSettings.Multiplex) == "" {
			common.ProtocolSettings.Multiplex = "off"
		}
		if common.ProtocolSettings.PaddingMin < 0 || common.ProtocolSettings.PaddingMin > 100 {
			return nil, fmt.Errorf("invalid Sudoku padding_min")
		}
		if common.ProtocolSettings.PaddingMax < common.ProtocolSettings.PaddingMin || common.ProtocolSettings.PaddingMax > 100 {
			return nil, fmt.Errorf("invalid Sudoku padding_max")
		}
		if err := validateSudokuProtocolSettings(&common.ProtocolSettings); err != nil {
			return nil, err
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

func validateSudokuProtocolSettings(settings *ProtocolSettings) error {
	if settings == nil {
		return fmt.Errorf("Sudoku protocol settings are missing")
	}
	switch strings.ToLower(strings.TrimSpace(settings.AEADMethod)) {
	case "aes-128-gcm", "chacha20-poly1305", "none":
	default:
		return fmt.Errorf("invalid Sudoku aead_method: %s", settings.AEADMethod)
	}
	table := strings.ToLower(strings.TrimSpace(settings.TableType))
	if table == "prefer_numeric" {
		table = "prefer_ascii"
	} else if table == "custom" {
		table = "prefer_entropy"
	}
	switch table {
	case "prefer_ascii", "prefer_entropy", "up_ascii_down_entropy", "up_entropy_down_ascii":
		settings.TableType = table
	default:
		return fmt.Errorf("invalid Sudoku table_type: %s", settings.TableType)
	}
	mode := strings.ToLower(strings.TrimSpace(settings.HTTPMaskMode))
	if mode == "split-stream" {
		mode = "stream"
	}
	switch mode {
	case "legacy", "stream", "poll", "auto", "ws":
		settings.HTTPMaskMode = mode
	default:
		return fmt.Errorf("invalid Sudoku http_mask_mode: %s", settings.HTTPMaskMode)
	}
	multiplex := strings.ToLower(strings.TrimSpace(settings.Multiplex))
	if multiplex == "low" {
		multiplex = "auto"
	} else if multiplex == "high" {
		multiplex = "on"
	}
	switch multiplex {
	case "off", "auto", "on":
		settings.Multiplex = multiplex
	default:
		return fmt.Errorf("invalid Sudoku multiplex: %s", settings.Multiplex)
	}
	if pathRoot := strings.Trim(settings.PathRoot, " /\t\n\r\x00\v"); pathRoot != "" {
		for i := 0; i < len(pathRoot); i++ {
			ch := pathRoot[i]
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
				return fmt.Errorf("invalid Sudoku path_root: contains invalid character %q", ch)
			}
		}
		settings.PathRoot = pathRoot
	}
	if err := validateSudokuTablePattern(settings.CustomTable); err != nil {
		return err
	}
	for index, pattern := range settings.CustomTables {
		if err := validateSudokuTablePattern(pattern); err != nil {
			return fmt.Errorf("invalid Sudoku custom_tables[%d]: %w", index, err)
		}
	}
	return nil
}

func validateSudokuTablePattern(pattern string) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	if len(pattern) != 8 {
		return fmt.Errorf("custom table must contain exactly 8 x/v/p characters")
	}
	var counts [3]int
	for _, ch := range strings.ToLower(pattern) {
		switch ch {
		case 'x':
			counts[0]++
		case 'v':
			counts[1]++
		case 'p':
			counts[2]++
		default:
			return fmt.Errorf("custom table contains invalid character %q", ch)
		}
	}
	if counts != [3]int{2, 4, 2} {
		return fmt.Errorf("custom table must contain 2 x, 4 v, and 2 p characters")
	}
	return nil
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

func isSupportedTLSCertMode(value string) bool {
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
