// Package shared contains runtime services that are independent of a
// protocol kernel. Kernel adapters authenticate users and hand accepted
// streams or sessions to RuntimeServices; policy, accounting and reporting
// remain identical for every protocol.
package shared

import (
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
	"github.com/limo13660/daonode/common/rate"
	"github.com/limo13660/daonode/limiter"
)

// TrafficTotal is the cumulative traffic counter for one user.
type TrafficTotal struct {
	Upload   int64
	Download int64
}

type trafficCounter struct {
	upload   atomic.Int64
	download atomic.Int64
}

func (c *trafficCounter) total() TrafficTotal {
	return TrafficTotal{Upload: c.upload.Load(), Download: c.download.Load()}
}

type trafficKey struct {
	tag string
	uid int
}

type pendingTraffic struct {
	total    TrafficTotal
	reported panel.UserTraffic
}

// committedTraffic survives an in-process runtime reload. Without this
// process-wide baseline, a replacement runtime could report old cumulative
// counters again.
var committedTraffic sync.Map // trafficKey -> TrafficTotal

// RuntimeServices implements user lookup, traffic reporting, rate limiting,
// device limits and active connection tracking for all protocol kernels.
type RuntimeServices struct {
	tag string

	mu           sync.Mutex
	users        map[int]panel.UserInfo
	usersByUUID  map[string]int
	counters     map[int]*trafficCounter
	pending      map[int]pendingTraffic
	connections  map[int]map[*Session]struct{}
	trafficUsers map[int]struct{}
}

// NewRuntimeServices creates the common services for one node runtime.
func NewRuntimeServices(tag string) *RuntimeServices {
	return &RuntimeServices{
		tag:          tag,
		users:        make(map[int]panel.UserInfo),
		usersByUUID:  make(map[string]int),
		counters:     make(map[int]*trafficCounter),
		pending:      make(map[int]pendingTraffic),
		connections:  make(map[int]map[*Session]struct{}),
		trafficUsers: make(map[int]struct{}),
	}
}

// SyncUsers updates the users accepted by the common connection policy. It
// must be called only after the kernel has applied the same user transaction.
func (s *RuntimeServices) SyncUsers(deleted, added []panel.UserInfo) {
	connections := make([]*Session, 0)

	s.mu.Lock()
	for _, user := range deleted {
		current, ok := s.users[user.Id]
		if ok {
			delete(s.usersByUUID, normalizedUUID(current.Uuid))
		}
		delete(s.users, user.Id)
		s.trafficUsers[user.Id] = struct{}{}
		for session := range s.connections[user.Id] {
			connections = append(connections, session)
		}
		delete(s.connections, user.Id)
	}
	for _, user := range added {
		if previous, ok := s.users[user.Id]; ok && previous.Uuid != user.Uuid {
			delete(s.usersByUUID, normalizedUUID(previous.Uuid))
		}
		s.users[user.Id] = user
		s.usersByUUID[normalizedUUID(user.Uuid)] = user.Id
		s.counterLocked(user.Id)
		// Seed the candidate set so an in-process runtime reload can recover
		// traffic accumulated before the replacement runtime was created.
		s.trafficUsers[user.Id] = struct{}{}
	}
	s.mu.Unlock()

	closeSessions(connections)
}

// UserByID returns a current panel user.
func (s *RuntimeServices) UserByID(uid int) (panel.UserInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[uid]
	return user, ok
}

// UserByUUID returns a current panel user by the credential used by UUID-based
// kernels such as Juicity.
func (s *RuntimeServices) UserByUUID(uuid string) (panel.UserInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.usersByUUID[normalizedUUID(uuid)]
	if !ok {
		return panel.UserInfo{}, false
	}
	user, ok := s.users[uid]
	return user, ok && normalizedUUID(user.Uuid) == normalizedUUID(uuid)
}

func normalizedUUID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// OpenConnection applies all common policy and returns a stream that counts
// client-to-server reads as upload and server-to-client writes as download.
// Closing either the returned stream or its Session releases the device slot.
// The release callback remains available for kernels whose upstream closes
// the original connection directly.
func (s *RuntimeServices) OpenConnection(user panel.UserInfo, conn net.Conn, trackDevice bool) (net.Conn, func(), bool) {
	if conn == nil {
		return nil, nil, false
	}
	session, accepted := s.openSession(user, conn, remoteIP(conn.RemoteAddr()), trackDevice)
	if !accepted {
		return nil, nil, false
	}
	stream := net.Conn(conn)
	if session.bucket != nil {
		stream = rate.NewConnRateLimiter(stream, session.bucket)
	}
	return &accountedConn{Conn: stream, session: session}, session.Release, true
}

// OpenSession applies the same user, device and speed policy for kernels that
// do not expose a net.Conn. The kernel records payload bytes on the returned
// session and closes it when the protocol session ends.
func (s *RuntimeServices) OpenSession(user panel.UserInfo, closer io.Closer, sourceAddress string, trackDevice bool) (*Session, bool) {
	if closer == nil {
		return nil, false
	}
	return s.openSession(user, closer, remoteIPString(sourceAddress), trackDevice)
}

func (s *RuntimeServices) openSession(user panel.UserInfo, closer io.Closer, ip string, trackDevice bool) (*Session, bool) {
	if !s.userIsCurrent(user) {
		return nil, false
	}

	nodeLimiter, err := limiter.GetLimiter(s.tag)
	if err != nil {
		return nil, false
	}
	userTag := format.UserTag(s.tag, user.Uuid)
	bucket, reject := nodeLimiter.CheckLimit(userTag, ip, trackDevice)
	if reject {
		return nil, false
	}

	s.mu.Lock()
	current, currentUser := s.users[user.Id]
	if !currentUser || current.Uuid != user.Uuid {
		s.mu.Unlock()
		if trackDevice {
			nodeLimiter.ReleaseConnection(userTag, ip)
		}
		return nil, false
	}
	session := &Session{
		service:     s,
		uid:         user.Id,
		closer:      closer,
		bucket:      bucket,
		nodeLimiter: nodeLimiter,
		userTag:     userTag,
		ip:          ip,
		trackDevice: trackDevice,
		counter:     s.counterLocked(user.Id),
	}
	connections := s.connections[user.Id]
	if connections == nil {
		connections = make(map[*Session]struct{})
		s.connections[user.Id] = connections
	}
	connections[session] = struct{}{}
	s.trafficUsers[user.Id] = struct{}{}
	s.mu.Unlock()
	return session, true
}

// Session is the common lifecycle and accounting handle for one authenticated
// protocol session. It is safe to record traffic from concurrent goroutines.
type Session struct {
	service *RuntimeServices
	uid     int
	closer  io.Closer
	bucket  *rate.DynamicBucket
	counter *trafficCounter

	nodeLimiter *limiter.Limiter
	userTag     string
	ip          string
	trackDevice bool
	releaseOnce sync.Once
	closeOnce   sync.Once
	closeErr    error
}

// RecordUpload adds client-to-server payload bytes.
func (s *Session) RecordUpload(bytes int64) {
	if s != nil && bytes > 0 {
		s.counter.upload.Add(bytes)
	}
}

// RecordDownload adds server-to-client payload bytes.
func (s *Session) RecordDownload(bytes int64) {
	if s != nil && bytes > 0 {
		s.counter.download.Add(bytes)
	}
}

// WaitUpload applies the node speed limit before an upload operation.
func (s *Session) WaitUpload(bytes int64) {
	if s != nil && s.bucket != nil && bytes > 0 {
		s.bucket.Get().Wait(bytes)
	}
}

// WaitDownload applies the node speed limit before a download operation.
func (s *Session) WaitDownload(bytes int64) {
	if s != nil && s.bucket != nil && bytes > 0 {
		s.bucket.Get().Wait(bytes)
	}
}

// Release removes the session from connection and online-device tracking. It
// is idempotent and does not close the protocol-owned resource.
func (s *Session) Release() {
	if s == nil {
		return
	}
	s.releaseOnce.Do(func() {
		s.service.mu.Lock()
		connections := s.service.connections[s.uid]
		delete(connections, s)
		if len(connections) == 0 {
			delete(s.service.connections, s.uid)
		}
		s.service.mu.Unlock()
		if s.trackDevice {
			s.nodeLimiter.ReleaseConnection(s.userTag, s.ip)
		}
	})
}

// Close closes the protocol-owned resource and releases common tracking.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.closer.Close()
		s.Release()
	})
	return s.closeErr
}

type accountedConn struct {
	net.Conn
	session *Session
}

func (c *accountedConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	c.session.RecordUpload(int64(n))
	return n, err
}

func (c *accountedConn) Write(buffer []byte) (int, error) {
	n, err := c.Conn.Write(buffer)
	c.session.RecordDownload(int64(n))
	return n, err
}

func (c *accountedConn) Close() error {
	return c.session.Close()
}

// CloseUserConnections closes all tracked connections for one user without
// holding the service lock while invoking a kernel or network callback.
func (s *RuntimeServices) CloseUserConnections(uid int) {
	closeSessions(s.takeConnections(uid))
}

// CloseAllConnections closes all connections tracked by this runtime.
func (s *RuntimeServices) CloseAllConnections() {
	s.mu.Lock()
	connections := make([]*Session, 0)
	for uid, active := range s.connections {
		for session := range active {
			connections = append(connections, session)
		}
		delete(s.connections, uid)
	}
	s.mu.Unlock()
	closeSessions(connections)
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
		counter := s.counters[uid]
		if counter == nil {
			continue
		}
		current := counter.total()
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

// RecordUpload adds traffic outside an opened Session. Protocol adapters
// should prefer Session.RecordUpload so active-user tracking remains exact.
func (s *RuntimeServices) RecordUpload(uid int, bytes int64) bool {
	return s.record(uid, bytes, true)
}

// RecordDownload adds traffic outside an opened Session.
func (s *RuntimeServices) RecordDownload(uid int, bytes int64) bool {
	return s.record(uid, bytes, false)
}

func (s *RuntimeServices) record(uid int, bytes int64, upload bool) bool {
	if bytes <= 0 {
		return false
	}
	s.mu.Lock()
	if _, ok := s.users[uid]; !ok {
		s.mu.Unlock()
		return false
	}
	counter := s.counterLocked(uid)
	s.trafficUsers[uid] = struct{}{}
	s.mu.Unlock()
	if upload {
		counter.upload.Add(bytes)
	} else {
		counter.download.Add(bytes)
	}
	return true
}

func (s *RuntimeServices) userIsCurrent(user panel.UserInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.users[user.Id]
	return ok && current.Uuid == user.Uuid
}

func (s *RuntimeServices) counterLocked(uid int) *trafficCounter {
	counter := s.counters[uid]
	if counter == nil {
		counter = &trafficCounter{}
		s.counters[uid] = counter
	}
	return counter
}

func (s *RuntimeServices) takeConnections(uid int) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.connections[uid]
	connections := make([]*Session, 0, len(active))
	for session := range active {
		connections = append(connections, session)
	}
	delete(s.connections, uid)
	return connections
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

func closeSessions(sessions []*Session) {
	for _, session := range sessions {
		_ = session.Close()
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

func remoteIPString(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

var _ net.Conn = (*accountedConn)(nil)
var _ io.Closer = (*Session)(nil)
