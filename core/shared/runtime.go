// Package shared contains runtime services that are independent of a
// protocol kernel. Kernel adapters provide authentication and cumulative
// traffic counters, then delegate connection policy and traffic reporting to
// RuntimeServices.
package shared

import (
	"fmt"
	"net"
	"sync"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
	"github.com/limo13660/daonode/common/rate"
	"github.com/limo13660/daonode/limiter"
)

// TrafficTotal is a kernel's cumulative traffic counter for one user.
type TrafficTotal struct {
	Upload   int64
	Download int64
}

// CounterSource returns the latest cumulative traffic counter for a user.
// Counters may reset when an upstream kernel resets its metrics.
type CounterSource func(uid int) (TrafficTotal, error)

type trafficKey struct {
	tag string
	uid int
}

type pendingTraffic struct {
	total    TrafficTotal
	reported panel.UserTraffic
}

// committedTraffic survives an in-process runtime reload. Without this
// process-wide baseline, a replacement runtime would report the kernel's
// existing cumulative counters again.
var committedTraffic sync.Map // trafficKey -> TrafficTotal

// RuntimeServices implements traffic reporting, rate limiting, device limits
// and active connection tracking for all protocol kernels.
type RuntimeServices struct {
	tag    string
	source CounterSource

	mu           sync.Mutex
	users        map[int]panel.UserInfo
	pending      map[int]pendingTraffic
	connections  map[int]map[net.Conn]struct{}
	trafficUsers map[int]struct{}
}

// NewRuntimeServices creates the common services for one node runtime.
func NewRuntimeServices(tag string, source CounterSource) *RuntimeServices {
	return &RuntimeServices{
		tag:          tag,
		source:       source,
		users:        make(map[int]panel.UserInfo),
		pending:      make(map[int]pendingTraffic),
		connections:  make(map[int]map[net.Conn]struct{}),
		trafficUsers: make(map[int]struct{}),
	}
}

// SyncUsers updates the users accepted by the common connection policy. It
// must be called only after the kernel has applied the same user transaction.
func (s *RuntimeServices) SyncUsers(deleted, added []panel.UserInfo) {
	connections := make([]net.Conn, 0)

	s.mu.Lock()
	for _, user := range deleted {
		delete(s.users, user.Id)
		s.trafficUsers[user.Id] = struct{}{}
		for conn := range s.connections[user.Id] {
			connections = append(connections, conn)
		}
		delete(s.connections, user.Id)
	}
	for _, user := range added {
		s.users[user.Id] = user
		// Seed the candidate set so an in-process runtime reload can recover
		// traffic accumulated before the replacement runtime was created.
		s.trafficUsers[user.Id] = struct{}{}
	}
	s.mu.Unlock()

	closeConnections(connections)
}

// OpenConnection applies the node and user limits, tracks the connection and
// returns a rate-limited stream when a speed limit is configured. The release
// callback is idempotent and must be called when the kernel finishes serving
// the connection.
func (s *RuntimeServices) OpenConnection(user panel.UserInfo, conn net.Conn, trackDevice bool) (net.Conn, func(), bool) {
	if conn == nil || !s.userIsCurrent(user) {
		return nil, nil, false
	}

	nodeLimiter, err := limiter.GetLimiter(s.tag)
	if err != nil {
		return nil, nil, false
	}
	userTag := format.UserTag(s.tag, user.Uuid)
	ip := remoteIP(conn.RemoteAddr())
	bucket, reject := nodeLimiter.CheckLimit(userTag, ip, trackDevice)
	if reject {
		return nil, nil, false
	}

	s.mu.Lock()
	current, currentUser := s.users[user.Id]
	if !currentUser || current.Uuid != user.Uuid {
		s.mu.Unlock()
		if trackDevice {
			nodeLimiter.ReleaseConnection(userTag, ip)
		}
		return nil, nil, false
	}
	connections := s.connections[user.Id]
	if connections == nil {
		connections = make(map[net.Conn]struct{})
		s.connections[user.Id] = connections
	}
	connections[conn] = struct{}{}
	s.trafficUsers[user.Id] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			connections := s.connections[user.Id]
			delete(connections, conn)
			if len(connections) == 0 {
				delete(s.connections, user.Id)
			}
			s.mu.Unlock()
			if trackDevice {
				nodeLimiter.ReleaseConnection(userTag, ip)
			}
		})
	}

	if bucket != nil {
		conn = rate.NewConnRateLimiter(conn, bucket)
	}
	return conn, release, true
}

// CloseUserConnections closes all tracked connections for one user without
// holding the service lock while invoking a kernel or network callback.
func (s *RuntimeServices) CloseUserConnections(uid int) {
	connections := s.takeConnections(uid)
	closeConnections(connections)
}

// CloseAllConnections closes all connections tracked by this runtime.
func (s *RuntimeServices) CloseAllConnections() {
	s.mu.Lock()
	connections := make([]net.Conn, 0)
	for uid, active := range s.connections {
		for conn := range active {
			connections = append(connections, conn)
		}
		delete(s.connections, uid)
	}
	s.mu.Unlock()
	closeConnections(connections)
}

// Traffic returns uncommitted traffic above the requested threshold. Removed
// users bypass the threshold so their final bytes can be flushed.
func (s *RuntimeServices) Traffic(minTraffic int) ([]panel.UserTraffic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var threshold int64
	if minTraffic > 0 {
		threshold = int64(minTraffic) * 1000
	}
	result := make([]panel.UserTraffic, 0, len(s.trafficUsers))
	for uid := range s.trafficUsers {
		current, err := s.loadTraffic(uid)
		if err != nil {
			return nil, fmt.Errorf("load traffic for user %d: %w", uid, err)
		}
		committedValue, _ := committedTraffic.LoadOrStore(trafficKey{tag: s.tag, uid: uid}, TrafficTotal{})
		committed := committedValue.(TrafficTotal)
		upload := trafficDelta(current.Upload, committed.Upload)
		download := trafficDelta(current.Download, committed.Download)
		_, currentUser := s.users[uid]
		active := len(s.connections[uid]) > 0
		if upload == 0 && download == 0 {
			if !active {
				delete(s.trafficUsers, uid)
				delete(s.pending, uid)
			}
			continue
		}
		effectiveThreshold := threshold
		if !currentUser {
			effectiveThreshold = 0
		}
		if upload+download <= effectiveThreshold {
			continue
		}
		reported := panel.UserTraffic{
			UID:         uid,
			Upload:      upload,
			Download:    download,
			ForceReport: !currentUser,
		}
		s.pending[uid] = pendingTraffic{total: current, reported: reported}
		result = append(result, reported)
	}
	return result, nil
}

// CommitTraffic advances cumulative baselines only for the exact snapshot
// that the panel accepted. Failed or stale reports remain eligible for retry.
func (s *RuntimeServices) CommitTraffic(traffic []panel.UserTraffic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range traffic {
		pending, ok := s.pending[item.UID]
		if !ok || pending.reported != item {
			continue
		}
		committedTraffic.Store(trafficKey{tag: s.tag, uid: item.UID}, pending.total)
		delete(s.pending, item.UID)
	}
}

func (s *RuntimeServices) userIsCurrent(user panel.UserInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.users[user.Id]
	return ok && current.Uuid == user.Uuid
}

func (s *RuntimeServices) takeConnections(uid int) []net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.connections[uid]
	connections := make([]net.Conn, 0, len(active))
	for conn := range active {
		connections = append(connections, conn)
	}
	delete(s.connections, uid)
	return connections
}

func (s *RuntimeServices) loadTraffic(uid int) (TrafficTotal, error) {
	if s.source == nil {
		return TrafficTotal{}, nil
	}
	return s.source(uid)
}

func trafficDelta(current, committed int64) int64 {
	if current < 0 {
		return 0
	}
	if current < committed {
		return current
	}
	return current - committed
}

func closeConnections(connections []net.Conn) {
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
