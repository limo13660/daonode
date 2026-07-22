package contract

import (
	"errors"

	panel "github.com/limo13660/daonode/api/v2board"
)

// ErrRuntimeStopTimeout means a kernel runtime can no longer be safely
// recovered in-process. The service supervisor should replace the process.
var ErrRuntimeStopTimeout = errors.New("core runtime stop timed out")

// Runtime is the lifecycle contract implemented by every core/<kernel>
// adapter. Traffic and CommitTraffic should normally be supplied by embedding
// core/shared.RuntimeServices.
type Runtime interface {
	Start() error
	Stop() error
	AddUsers([]panel.UserInfo) (int, error)
	DelUsers([]panel.UserInfo) error
	SyncUsers([]panel.UserInfo, []panel.UserInfo) error
	Traffic(int) ([]panel.UserTraffic, error)
	CommitTraffic([]panel.UserTraffic)
}

// Validator is implemented by runtimes that can fully construct and validate
// a candidate configuration without binding its listening sockets. Reloads
// use it before stopping the active runtime.
type Validator interface {
	Validate([]panel.UserInfo) error
}
