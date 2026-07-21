package panel

import "testing"

func TestBuildPanelUserNameContract(t *testing.T) {
	if got := BuildPanelUserName("ysbl-panel", 42); got != "ysbl-panel-42" {
		t.Fatalf("username = %q, want ysbl-panel-42", got)
	}
	if first, second := BuildPanelUserName("ysbl-panel", 42), BuildPanelUserName("ysbl-panel", 43); first == second {
		t.Fatalf("different user IDs received the same username %q", first)
	}
}
