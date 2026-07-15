package contract

import (
	"errors"

	panel "github.com/limo13660/daonode/api/v2board"
)

// ErrRuntimeStopTimeout means a kernel runtime can no longer be safely
// recovered in-process. The service supervisor should replace the process.
var ErrRuntimeStopTimeout = errors.New("core runtime stop timed out")

// Runtime is the lifecycle contract implemented by every core/<kernel>
// adapter.
type Runtime interface {
	Start() error
	Stop() error
	AddUsers([]panel.UserInfo) (int, error)
	DelUsers([]panel.UserInfo) error
	SyncUsers([]panel.UserInfo, []panel.UserInfo) error
	Traffic(int) ([]panel.UserTraffic, error)
	CommitTraffic([]panel.UserTraffic)
}
