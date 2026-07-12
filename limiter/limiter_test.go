package limiter

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
)

func TestRenewedUserRestoresLimits(t *testing.T) {
	Init()
	const tag = "renew-limits"
	user := panel.UserInfo{Id: 7, Uuid: "renew-limit-user", SpeedLimit: 20, DeviceLimit: 2}
	limiter := AddLimiter("mieru", tag, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { DeleteLimiter(tag) })

	bucket, reject := limiter.CheckLimit(format.UserTag(tag, user.Uuid), "198.51.100.1", true)
	if reject || bucket == nil {
		t.Fatalf("initial user limit = bucket:%v reject:%v", bucket, reject)
	}
	if got, want := bucket.Get().Rate(), float64(20*1000000/8); got != want {
		t.Fatalf("initial speed rate = %v, want %v", got, want)
	}

	limiter.UpdateUser(tag, nil, []panel.UserInfo{user}, nil)
	if _, reject := limiter.CheckLimit(format.UserTag(tag, user.Uuid), "198.51.100.1", true); !reject {
		t.Fatal("expired user was not rejected by limiter")
	}

	renewed := user
	renewed.SpeedLimit = 10
	renewed.DeviceLimit = 1
	limiter.UpdateUser(tag, []panel.UserInfo{renewed}, nil, nil)
	bucket, reject = limiter.CheckLimit(format.UserTag(tag, renewed.Uuid), "198.51.100.2", true)
	if reject || bucket == nil {
		t.Fatalf("renewed user limit = bucket:%v reject:%v", bucket, reject)
	}
	if got, want := bucket.Get().Rate(), float64(10*1000000/8); got != want {
		t.Fatalf("renewed speed rate = %v, want %v", got, want)
	}
	limiter.SetAliveList(map[int]int{renewed.Id: 1})
	if _, reject := limiter.CheckLimit(format.UserTag(tag, renewed.Uuid), "198.51.100.3", true); !reject {
		t.Fatal("renewed user device limit was not enforced")
	}
}

func TestModifiedUserUpdatesSpeedAndDeviceLimit(t *testing.T) {
	Init()
	const tag = "modify-limits"
	user := panel.UserInfo{Id: 8, Uuid: "modify-limit-user", SpeedLimit: 20, DeviceLimit: 2}
	limiter := AddLimiter("mieru", tag, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { DeleteLimiter(tag) })

	modified := user
	modified.SpeedLimit = 5
	modified.DeviceLimit = 1
	limiter.UpdateUser(tag, nil, nil, []panel.UserInfo{modified})

	value, ok := limiter.UserLimitInfo.Load(format.UserTag(tag, user.Uuid))
	if !ok {
		t.Fatal("modified user limit info is missing")
	}
	info := value.(*UserLimitInfo)
	if info.SpeedLimit != 5 || info.DeviceLimit != 1 {
		t.Fatalf("modified user limits = speed:%d device:%d", info.SpeedLimit, info.DeviceLimit)
	}
	bucket, reject := limiter.CheckLimit(format.UserTag(tag, user.Uuid), "203.0.113.1", true)
	if reject || bucket == nil {
		t.Fatalf("modified user limit = bucket:%v reject:%v", bucket, reject)
	}
	if got, want := bucket.Get().Rate(), float64(5*1000000/8); got != want {
		t.Fatalf("modified speed rate = %v, want %v", got, want)
	}
}

func TestDeviceLimitCountsNewIPsWithinSameInterval(t *testing.T) {
	Init()
	const tag = "device-window"
	user := panel.UserInfo{Id: 11, Uuid: "device-user", DeviceLimit: 2}
	limiter := AddLimiter("mieru", tag, []panel.UserInfo{user}, map[int]int{user.Id: 1})
	t.Cleanup(func() { DeleteLimiter(tag) })

	if _, reject := limiter.CheckLimit(format.UserTag(tag, user.Uuid), "198.51.100.10", true); reject {
		t.Fatal("first new IP was rejected below the device limit")
	}
	if _, reject := limiter.CheckLimit(format.UserTag(tag, user.Uuid), "198.51.100.11", true); !reject {
		t.Fatal("second new IP was accepted above the device limit in one interval")
	}
}
