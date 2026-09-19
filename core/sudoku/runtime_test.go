package sudoku

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	transport "github.com/limo13660/daonode/core/sudoku/transport"
	"github.com/limo13660/daonode/limiter"
)

func TestBuildUserConfigsUsesUUIDKeysAndPreservesSettings(t *testing.T) {
	info := sudokuNodeInfo()
	info.Common.ProtocolSettings = panel.ProtocolSettings{
		AEADMethod:         "CHACHA20-POLY1305",
		PaddingMin:         0,
		PaddingMax:         0,
		TableType:          "prefer_entropy",
		EnablePureDownlink: true,
		HTTPMask:           true,
		HTTPMaskMode:       "ws",
		HTTPMaskTLS:        true,
		HTTPMaskHost:       "cdn.example.com",
		PathRoot:           "sudoku",
		Multiplex:          "on",
		CustomTable:        "xxppvvvv",
		CustomTables:       []string{"xxppvvvv", "xpxpvvvv"},
	}
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		2: {Id: 2, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
	}

	snapshot, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("buildUserConfigs() error = %v", err)
	}
	if len(snapshot.byHash) != len(users) {
		t.Fatalf("user configs = %d, want %d", len(snapshot.byHash), len(users))
	}
	for _, user := range users {
		hash := transport.KIPUserHashHexFromKey(user.Uuid)
		entry, ok := snapshot.byHash[hash]
		if !ok {
			t.Fatalf("missing config for user %d hash %s", user.Id, hash)
		}
		if entry.user != user {
			t.Fatalf("stored user = %#v, want %#v", entry.user, user)
		}
		cfg := entry.cfg
		if cfg.Key != user.Uuid {
			t.Fatalf("user %d key = %q, want UUID", user.Id, cfg.Key)
		}
		if cfg.AEADMethod != "chacha20-poly1305" || cfg.PaddingMin != 0 || cfg.PaddingMax != 0 {
			t.Fatalf("user %d crypto/padding = %#v", user.Id, cfg)
		}
		if !cfg.EnablePureDownlink || cfg.DisableHTTPMask || cfg.HTTPMaskMode != "ws" ||
			!cfg.HTTPMaskTLSEnabled || cfg.HTTPMaskHost != "cdn.example.com" ||
			cfg.HTTPMaskPathRoot != "sudoku" || cfg.MultiplexMode() != "on" {
			t.Fatalf("user %d HTTPMask settings = %#v", user.Id, cfg)
		}
		if len(cfg.Tables) < 2 {
			t.Fatalf("user %d tables = %d, want custom table candidates", user.Id, len(cfg.Tables))
		}
	}
}

func TestBuildUserConfigsAcceptsShadowrocketDefaultTable(t *testing.T) {
	info := sudokuNodeInfo()
	info.Common.ProtocolSettings.TableType = "prefer_ascii"
	info.Common.ProtocolSettings.CustomTable = ""
	info.Common.ProtocolSettings.CustomTables = nil
	users := map[int]panel.UserInfo{1: {Id: 1, Uuid: "shadowrocket-user"}}

	snapshot, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("buildUserConfigs() error = %v", err)
	}
	entry := snapshot.byHash[transport.KIPUserHashHexFromKey("shadowrocket-user")]
	if entry.cfg == nil || len(entry.cfg.Tables) < 2 {
		t.Fatalf("table candidates = %#v, want configured and compatibility tables", entry.cfg)
	}
	if entry.cfg.Tables[0].Hint() == entry.cfg.Tables[1].Hint() {
		t.Fatal("configured and compatibility tables have the same hint")
	}
}

func TestCompileSudokuRoutesSupportsGeoIPPrivate(t *testing.T) {
	policy, err := compileRoutePolicy([]panel.Route{
		{Id: 4, Action: "block_ip", Match: []string{"geoip:private"}},
		{Id: 5, Action: "block_port", Match: []string{"25,6881-6889"}},
	})
	if err != nil {
		t.Fatalf("compileRoutePolicy() error = %v", err)
	}
	if !policy.blocked("", "192.168.1.10", 443) {
		t.Fatal("geoip:private did not block RFC1918 address")
	}
	if !policy.blocked("", "100.64.1.1", 443) {
		t.Fatal("geoip:private did not block CGNAT address")
	}
	if policy.blocked("", "8.8.8.8", 443) {
		t.Fatal("geoip:private blocked a public address")
	}
	if !policy.blocked("", "8.8.8.8", 6885) {
		t.Fatal("comma/range port matcher did not block port")
	}
}

func TestRuntimeUserLifecycleKeepsListenerAndRotatesUUIDKeys(t *testing.T) {
	limiter.Init()
	info := sudokuNodeInfo()
	info.Common.ServerPort = reserveSudokuPort(t)
	tag := fmt.Sprintf("sudoku-lifecycle-%d", info.Common.ServerPort)
	users := []panel.UserInfo{{Id: 1, Uuid: "old-sudoku-user"}}
	limiter.AddLimiter("sudoku", tag, 0, users, nil)
	defer limiter.DeleteLimiter(tag)

	runtime := NewRuntime(tag, info).(*runtime)
	if _, err := runtime.AddUsers(users); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	if runtime.instance == nil {
		t.Fatal("Sudoku listener was not started after adding a user")
	}
	if err := runtime.Start(); err != nil {
		t.Fatalf("idempotent Start() error = %v", err)
	}
	assertSudokuPortListening(t, info.Common.ServerPort)

	old := users[0]
	updated := panel.UserInfo{Id: 1, Uuid: "new-sudoku-user"}
	if err := runtime.SyncUsers([]panel.UserInfo{old}, []panel.UserInfo{updated}); err != nil {
		t.Fatalf("SyncUsers() UUID rotation error = %v", err)
	}
	oldHash := transport.KIPUserHashHexFromKey(old.Uuid)
	newHash := transport.KIPUserHashHexFromKey(updated.Uuid)
	snapshot := runtime.instance.users.Load()
	if snapshot == nil {
		t.Fatal("user snapshot is nil after UUID rotation")
	}
	if _, ok := snapshot.byHash[oldHash]; ok {
		t.Fatal("old UUID key remained active after rotation")
	}
	if _, ok := snapshot.byHash[newHash]; !ok {
		t.Fatal("new UUID key was not activated after rotation")
	}
	assertSudokuPortListening(t, info.Common.ServerPort)

	if err := runtime.DelUsers([]panel.UserInfo{updated}); err != nil {
		t.Fatalf("DelUsers() error = %v", err)
	}
	if runtime.instance != nil {
		t.Fatal("Sudoku listener remained active after deleting the last user")
	}
	if err := runtime.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func reserveSudokuPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Sudoku port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func assertSudokuPortListening(t *testing.T, port int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("Sudoku port %d is not listening: %v", port, err)
	}
	_ = conn.Close()
}

func TestBuildUserConfigsReusesHTTPMaskSessionsForUnchangedUsers(t *testing.T) {
	info := sudokuNodeInfo()
	info.Common.ProtocolSettings.HTTPMask = true
	info.Common.ProtocolSettings.HTTPMaskMode = "poll"
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
	}

	first, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("buildUserConfigs() error = %v", err)
	}
	second, err := buildUserConfigsWithPrevious(info, users, first)
	if err != nil {
		t.Fatalf("buildUserConfigsWithPrevious() error = %v", err)
	}
	hash := transport.KIPUserHashHexFromKey(users[1].Uuid)
	if first.byHash[hash].tunnel == nil {
		t.Fatal("HTTPMask tunnel server was not created")
	}
	if second.byHash[hash].tunnel != first.byHash[hash].tunnel {
		t.Fatal("HTTPMask tunnel server was replaced during user sync")
	}
	if second.byHash[hash].cfg != first.byHash[hash].cfg {
		t.Fatal("Sudoku tables were rebuilt during an unchanged user sync")
	}
}

func TestBuildUserConfigsSharesHTTPMaskTunnelAcrossUsers(t *testing.T) {
	info := sudokuNodeInfo()
	info.Common.ProtocolSettings.HTTPMask = true
	info.Common.ProtocolSettings.HTTPMaskMode = "stream"
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}
	snapshot, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("buildUserConfigs() error = %v", err)
	}
	if len(snapshot.entries) != len(users) {
		t.Fatalf("entries = %d, want %d", len(snapshot.entries), len(users))
	}
	shared := snapshot.entries[0].tunnel
	if shared == nil {
		t.Fatal("HTTPMask tunnel was not created")
	}
	for i, entry := range snapshot.entries[1:] {
		if entry.tunnel != shared {
			t.Fatalf("user entry %d has a distinct HTTPMask tunnel", i+2)
		}
	}
}

func TestRuntimeStopClosesStalledSudokuHandshake(t *testing.T) {
	limiter.Init()
	info := sudokuNodeInfo()
	info.Common.ServerPort = reserveSudokuPort(t)
	tag := fmt.Sprintf("sudoku-stalled-stop-%d", info.Common.ServerPort)
	users := []panel.UserInfo{{Id: 1, Uuid: "stalled-sudoku-user"}}
	limiter.AddLimiter("sudoku", tag, 0, users, nil)
	defer limiter.DeleteLimiter(tag)

	runtime := NewRuntime(tag, info).(*runtime)
	if _, err := runtime.AddUsers(users); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Common.ServerPort), time.Second)
	if err != nil {
		t.Fatalf("dial Sudoku listener: %v", err)
	}
	defer conn.Close()

	acceptedDeadline := time.Now().Add(time.Second)
	for {
		runtime.instance.connMu.Lock()
		accepted := len(runtime.instance.conns) > 0
		runtime.instance.connMu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(acceptedDeadline) {
			t.Fatal("stalled handshake was not accepted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	started := time.Now()
	if err := runtime.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Stop() took %s with a stalled handshake", elapsed)
	}
}

func TestBuildUserConfigsRejectsInvalidUsers(t *testing.T) {
	tests := []struct {
		name  string
		users map[int]panel.UserInfo
		want  string
	}{
		{
			name:  "empty UUID",
			users: map[int]panel.UserInfo{1: {Id: 1, Uuid: "  "}},
			want:  "empty UUID",
		},
		{
			name: "duplicate UUID",
			users: map[int]panel.UserInfo{
				1: {Id: 1, Uuid: "same-key"},
				2: {Id: 2, Uuid: "same-key"},
			},
			want: "duplicated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildUserConfigs(sudokuNodeInfo(), test.users)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("buildUserConfigs() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestBuildUserConfigsRejectsInvalidProtocolSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*panel.NodeInfo)
		want   string
	}{
		{
			name: "non TCP transport",
			mutate: func(info *panel.NodeInfo) {
				info.Common.TransportProtocol = "UDP"
			},
			want: "must be TCP",
		},
		{
			name: "invalid AEAD",
			mutate: func(info *panel.NodeInfo) {
				info.Common.ProtocolSettings.AEADMethod = "aes-256-gcm"
			},
			want: "invalid aead-method",
		},
		{
			name: "invalid table",
			mutate: func(info *panel.NodeInfo) {
				info.Common.ProtocolSettings.TableType = "random"
			},
			want: "table-type",
		},
		{
			name: "invalid multiplex",
			mutate: func(info *panel.NodeInfo) {
				info.Common.ProtocolSettings.Multiplex = "always"
			},
			want: "invalid multiplex",
		},
		{
			name: "invalid path root",
			mutate: func(info *panel.NodeInfo) {
				info.Common.ProtocolSettings.PathRoot = "nested/path"
			},
			want: "path-root",
		},
		{
			name: "invalid padding",
			mutate: func(info *panel.NodeInfo) {
				info.Common.ProtocolSettings.PaddingMin = 40
				info.Common.ProtocolSettings.PaddingMax = 20
			},
			want: "padding-max",
		},
	}
	users := map[int]panel.UserInfo{1: {Id: 1, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := sudokuNodeInfo()
			test.mutate(info)
			_, err := buildUserConfigs(info, users)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("buildUserConfigs() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func sudokuNodeInfo() *panel.NodeInfo {
	return &panel.NodeInfo{
		Type:   "sudoku",
		Kernel: "sudoku",
		Common: &panel.CommonNode{
			Protocol:          "sudoku",
			Kernel:            "sudoku",
			ListenIP:          "127.0.0.1",
			ServerPort:        2087,
			TransportProtocol: "TCP",
			ProtocolSettings: panel.ProtocolSettings{
				AEADMethod: "chacha20-poly1305",
				PaddingMin: 10,
				PaddingMax: 30,
				TableType:  "prefer_entropy",
				Multiplex:  "off",
			},
		},
	}
}
