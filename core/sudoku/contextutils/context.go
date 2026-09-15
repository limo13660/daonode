package contextutils

import "context"

func AfterFunc(ctx context.Context, f func()) func() bool { return context.AfterFunc(ctx, f) }
