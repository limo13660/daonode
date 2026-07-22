package limiter

import (
	"errors"
	"maps"
	"strings"
	"sync"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
	"github.com/limo13660/daonode/common/rate"
)

var limitLock sync.RWMutex
var limiter map[string]*Limiter

func Init() {
	limiter = map[string]*Limiter{}
}

type Limiter struct {
	Nodetype         string              // Registered protocol runtime name.
	SpeedLimit       int                 // Aggregate node speed limit in Mbps.
	NodeSpeedLimiter *rate.DynamicBucket // Shared by every connection on the node.
	UserLimitInfo    *sync.Map           // Key: TagUUID value: UserLimitInfo
	AliveList        map[int]int         // Key: Uid, value: alive_ip
	userMu           sync.RWMutex
	aliveMu          sync.RWMutex
	onlineMu         sync.RWMutex
	online           map[string]*onlineUserState
	reported         map[string]map[string]struct{}
	pendingOffline   map[string]int
}

type onlineUserState struct {
	uid int
	ips map[string]int
}

type UserLimitInfo struct {
	UID         int
	DeviceLimit int
}

// OfflineUser identifies an exact locally-offline user snapshot. UserTag is
// retained so an acknowledgement cannot clear a newer reconnect for the same
// panel UID.
type OfflineUser struct {
	UserTag string
	UID     int
}

func AddLimiter(nodetype string, tag string, speedLimit int, users []panel.UserInfo, aliveList map[int]int) *Limiter {
	l := &Limiter{
		Nodetype:       nodetype,
		SpeedLimit:     speedLimit,
		UserLimitInfo:  new(sync.Map),
		AliveList:      maps.Clone(aliveList),
		online:         make(map[string]*onlineUserState),
		reported:       make(map[string]map[string]struct{}),
		pendingOffline: make(map[string]int),
	}
	if speedLimit > 0 {
		l.NodeSpeedLimiter = rate.NewDynamicBucket(int64(speedLimit) * 1000000 / 8)
	}
	for i := range users {
		userLimit := &UserLimitInfo{UID: users[i].Id}
		if users[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = users[i].DeviceLimit
		}
		l.UserLimitInfo.Store(format.UserTag(tag, users[i].Uuid), userLimit)
	}
	limitLock.Lock()
	limiter[tag] = l
	limitLock.Unlock()
	return l
}

func GetLimiter(tag string) (info *Limiter, err error) {
	limitLock.RLock()
	info, ok := limiter[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	delete(limiter, tag)
	limitLock.Unlock()
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo, modified []panel.UserInfo) {
	l.userMu.Lock()
	for i := range deleted {
		l.UserLimitInfo.Delete(format.UserTag(tag, deleted[i].Uuid))
	}
	for i := range modified {
		if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, modified[i].Uuid)); ok {
			u := v.(*UserLimitInfo)
			u.DeviceLimit = modified[i].DeviceLimit
		}
	}
	for i := range added {
		userLimit := &UserLimitInfo{
			UID: added[i].Id,
		}
		if added[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = added[i].DeviceLimit
		}
		l.UserLimitInfo.Store(format.UserTag(tag, added[i].Uuid), userLimit)
	}
	l.userMu.Unlock()

	l.aliveMu.Lock()
	for i := range deleted {
		delete(l.AliveList, deleted[i].Id)
	}
	l.aliveMu.Unlock()

	l.onlineMu.Lock()
	for i := range deleted {
		taguuid := format.UserTag(tag, deleted[i].Uuid)
		delete(l.online, taguuid)
		delete(l.reported, taguuid)
		delete(l.pendingOffline, taguuid)
	}
	l.onlineMu.Unlock()
}

func (l *Limiter) SetAliveList(aliveList map[int]int) {
	l.aliveMu.Lock()
	l.AliveList = maps.Clone(aliveList)
	l.aliveMu.Unlock()
}

func (l *Limiter) CheckLimit(taguuid string, ip string, noUDPsource bool) (DynamicBucket *rate.DynamicBucket, Reject bool) {
	l.userMu.RLock()
	// check if ipv4 mapped ipv6
	ip = strings.TrimPrefix(ip, "::ffff:")

	deviceLimit := 0
	var uid int
	if v, ok := l.UserLimitInfo.Load(taguuid); ok {
		u := v.(*UserLimitInfo)
		deviceLimit = u.DeviceLimit
		uid = u.UID
	} else {
		l.userMu.RUnlock()
		return nil, true
	}
	l.userMu.RUnlock()

	if noUDPsource {
		l.aliveMu.RLock()
		aliveIp := l.AliveList[uid]
		l.aliveMu.RUnlock()
		if !l.trackConnection(taguuid, uid, ip, aliveIp, deviceLimit) {
			return nil, true
		}
	}

	return l.NodeSpeedLimiter, false
}

func (l *Limiter) trackConnection(taguuid string, uid int, ip string, aliveIP, deviceLimit int) bool {
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()
	state := l.online[taguuid]
	if state == nil {
		state = &onlineUserState{uid: uid, ips: make(map[string]int)}
		l.online[taguuid] = state
	}
	if state.ips[ip] > 0 {
		state.ips[ip]++
		delete(l.pendingOffline, taguuid)
		return true
	}

	if deviceLimit > 0 {
		reported := l.reported[taguuid]
		// The panel count can still contain this runtime's last report after
		// the final local stream closes. Treat an immediate reconnect from the
		// exact reported IP as the same device, not as an additional device.
		if _, pending := l.pendingOffline[taguuid]; pending {
			if _, alreadyReported := reported[ip]; alreadyReported {
				state.ips[ip] = 1
				delete(l.pendingOffline, taguuid)
				return true
			}
		}
		reportedActive := 0
		unreportedActive := 0
		for activeIP := range state.ips {
			if _, ok := reported[activeIP]; ok {
				reportedActive++
			} else {
				unreportedActive++
			}
		}
		knownDevices := max(aliveIP, reportedActive)
		if knownDevices+unreportedActive+1 > deviceLimit {
			if len(state.ips) == 0 {
				delete(l.online, taguuid)
			}
			return false
		}
	}
	state.ips[ip] = 1
	delete(l.pendingOffline, taguuid)
	return true
}

// ReleaseConnection removes one accepted connection from the online device
// snapshot. Multiple concurrent connections from the same IP share one device
// slot and are removed only after the final connection closes.
func (l *Limiter) ReleaseConnection(taguuid, ip string) {
	ip = strings.TrimPrefix(ip, "::ffff:")
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()
	state := l.online[taguuid]
	if state == nil {
		return
	}
	if state.ips[ip] > 1 {
		state.ips[ip]--
		return
	}
	delete(state.ips, ip)
	if len(state.ips) == 0 {
		delete(l.online, taguuid)
		if len(l.reported[taguuid]) > 0 {
			l.pendingOffline[taguuid] = state.uid
		} else {
			delete(l.pendingOffline, taguuid)
		}
	}
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	l.onlineMu.RLock()
	defer l.onlineMu.RUnlock()
	var onlineUser []panel.OnlineUser
	for _, state := range l.online {
		for ip := range state.ips {
			onlineUser = append(onlineUser, panel.OnlineUser{UID: state.uid, IP: ip})
		}
	}

	return &onlineUser, nil
}

// GetPendingOfflineUsers returns users whose last locally-active IP was
// previously reported to the panel. Callers should submit these UIDs with an
// explicit empty IP array so the panel can remove the stale node contribution.
func (l *Limiter) GetPendingOfflineUsers() []OfflineUser {
	l.onlineMu.RLock()
	defer l.onlineMu.RUnlock()
	result := make([]OfflineUser, 0, len(l.pendingOffline))
	for taguuid, uid := range l.pendingOffline {
		result = append(result, OfflineUser{UserTag: taguuid, UID: uid})
	}
	return result
}

// MarkOfflineUsersReported clears only offline snapshots that are still
// current. A reconnect removes pendingOffline, so a delayed acknowledgement
// can never erase the newer online state.
func (l *Limiter) MarkOfflineUsersReported(offline []OfflineUser) {
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()
	for _, item := range offline {
		uid, pending := l.pendingOffline[item.UserTag]
		if !pending || uid != item.UID {
			continue
		}
		if state := l.online[item.UserTag]; state != nil && len(state.ips) > 0 {
			continue
		}
		delete(l.pendingOffline, item.UserTag)
		delete(l.reported, item.UserTag)
	}
}

// MarkOnlineDeviceReported records the exact snapshot accepted by the panel.
// A failed report leaves the previous snapshot intact so device-limit checks
// continue to match the panel's last known state.
func (l *Limiter) MarkOnlineDeviceReported(online []panel.OnlineUser) {
	byUID := make(map[int]map[string]struct{})
	for _, item := range online {
		ips := byUID[item.UID]
		if ips == nil {
			ips = make(map[string]struct{})
			byUID[item.UID] = ips
		}
		ips[strings.TrimPrefix(item.IP, "::ffff:")] = struct{}{}
	}

	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()
	for taguuid, state := range l.online {
		if ips := byUID[state.uid]; len(ips) > 0 {
			l.reported[taguuid] = maps.Clone(ips)
		} else {
			delete(l.reported, taguuid)
		}
	}
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}
