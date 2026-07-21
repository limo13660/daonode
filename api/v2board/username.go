package panel

import (
	"fmt"
	"strings"
)

// EffectivePanelIdentifier resolves the panel-wide identifier while retaining
// compatibility with DaoBoard releases that only emitted username_prefix.
func (c *CommonNode) EffectivePanelIdentifier(nodeID int) string {
	if c != nil {
		if identifier := strings.TrimSpace(c.PanelIdentifier); identifier != "" {
			return identifier
		}
		if identifier := strings.TrimSpace(c.UserNamePrefix); identifier != "" {
			return identifier
		}
	}
	return fmt.Sprintf("n%d", nodeID)
}

// BuildPanelUserName is the shared Mieru and NaiveProxy authentication
// contract: every node belonging to the same panel uses the same username for
// a given panel user ID.
func BuildPanelUserName(panelIdentifier string, userID int) string {
	return fmt.Sprintf("%s-%d", strings.TrimSpace(panelIdentifier), userID)
}
