package rate

import (
	"sync/atomic"
	"time"

	"github.com/juju/ratelimit"
)

type DynamicBucket struct {
	v atomic.Value // *ratelimit.Bucket
}

func NewDynamicBucket(rate int64) *DynamicBucket {
	d := &DynamicBucket{}
	d.Update(rate)
	return d
}

func (d *DynamicBucket) Get() *ratelimit.Bucket {
	return d.v.Load().(*ratelimit.Bucket)
}

func (d *DynamicBucket) Update(rate int64) {
	d.v.Store(ratelimit.NewBucketWithQuantum(time.Second, rate, rate))
}
