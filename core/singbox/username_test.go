package singbox

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestNaiveUsesPanelScopedUserName(t *testing.T) {
	runtime := &runtime{info: &panel.NodeInfo{
		Id: 99,
		Common: &panel.CommonNode{
			PanelIdentifier: "ysbl-panel",
			UserNamePrefix:  "legacy-node-prefix",
		},
	}}

	if got := runtime.userName(42); got != "ysbl-panel-42" {
		t.Fatalf("Naive username = %q, want ysbl-panel-42", got)
	}
}
