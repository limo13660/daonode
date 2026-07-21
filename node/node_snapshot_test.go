package node

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/conf"
)

func TestSnapshotIsIndependentAndRestorable(t *testing.T) {
	retryCount := 2
	actionValue := "relay.example:443"
	config := conf.NodeConfig{
		APIHost:    "https://panel.example",
		NodeID:     7,
		Key:        "secret",
		Timeout:    8,
		RetryCount: &retryCount,
	}
	info := &panel.NodeInfo{
		Id:       7,
		Type:     "naive",
		Kernel:   "singbox",
		Security: panel.Tls,
		Tag:      "node-7",
		Common: &panel.CommonNode{
			Protocol:   "naive",
			Kernel:     "singbox",
			ServerPort: 443,
			Routes: []panel.Route{{
				Id:          1,
				Match:       []string{"domain_suffix:example.com"},
				Action:      "route",
				ActionValue: &actionValue,
			}},
			TlsSettings: panel.TlsSettings{ServerNames: []string{"node.example"}},
			CertInfo: &panel.CertInfo{
				CertDomains: []string{"node.example"},
				DNSEnv:      map[string]string{"TOKEN": "value"},
			},
		},
	}
	controller := NewController(nil, &config, info)
	controller.userList = []panel.UserInfo{{Id: 10, Uuid: "uuid-10", DeviceLimit: 2}}
	controller.aliveMap = map[int]int{10: 1}
	controller.tag = info.Tag
	controller.prepared = true
	original := &Node{
		controllers: []*Controller{controller},
		NodeInfos:   []*panel.NodeInfo{info},
	}

	snapshot, err := original.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// Mutating the active objects must not corrupt the rollback copy.
	retryCount = 9
	controller.userList[0].Uuid = "changed"
	controller.aliveMap[10] = 4
	info.Common.Routes[0].Match[0] = "changed"
	*info.Common.Routes[0].ActionValue = "changed"
	info.Common.TlsSettings.ServerNames[0] = "changed"
	info.Common.CertInfo.CertDomains[0] = "changed"
	info.Common.CertInfo.DNSEnv["TOKEN"] = "changed"

	state := snapshot.Nodes[0]
	if state.Config.RetryCount == nil || *state.Config.RetryCount != 2 {
		t.Fatalf("snapshot retry count = %v, want 2", state.Config.RetryCount)
	}
	if state.Users[0].Uuid != "uuid-10" || state.Alive[10] != 1 {
		t.Fatalf("snapshot user state changed: users=%#v alive=%#v", state.Users, state.Alive)
	}
	if state.Info.Common.Routes[0].Match[0] != "domain_suffix:example.com" ||
		*state.Info.Common.Routes[0].ActionValue != "relay.example:443" ||
		state.Info.Common.TlsSettings.ServerNames[0] != "node.example" ||
		state.Info.Common.CertInfo.CertDomains[0] != "node.example" ||
		state.Info.Common.CertInfo.DNSEnv["TOKEN"] != "value" {
		t.Fatalf("snapshot node info was not deeply copied: %#v", state.Info.Common)
	}

	restored, err := NewFromSnapshot(snapshot)
	if err != nil {
		t.Fatalf("NewFromSnapshot() error = %v", err)
	}
	restoredSnapshot, err := restored.Snapshot()
	if err != nil {
		t.Fatalf("restored Snapshot() error = %v", err)
	}
	if got := restoredSnapshot.Nodes[0].Users[0].Uuid; got != "uuid-10" {
		t.Fatalf("restored user UUID = %q, want uuid-10", got)
	}
}
