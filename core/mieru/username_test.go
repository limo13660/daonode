package mieru

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestMieruUsesPanelScopedUserName(t *testing.T) {
	runtime := &mieruRuntime{info: &panel.NodeInfo{
		Id: 7,
		Common: &panel.CommonNode{
			PanelIdentifier: "ysbl-panel",
			UserNamePrefix:  "legacy-node-prefix",
		},
	}}

	if got := runtime.userName(42); got != "ysbl-panel-42" {
		t.Fatalf("Mieru username = %q, want ysbl-panel-42", got)
	}
}

func TestMieruKeepsLegacyUsernameUntilPanelIdentifierIsDownlinked(t *testing.T) {
	runtime := &mieruRuntime{info: &panel.NodeInfo{
		Id: 7,
		Common: &panel.CommonNode{
			UserNamePrefix: "legacy-node-prefix",
		},
	}}

	if got := runtime.userName(42); got != "legacy-node-prefixu42" {
		t.Fatalf("legacy Mieru username = %q, want legacy-node-prefixu42", got)
	}
}
