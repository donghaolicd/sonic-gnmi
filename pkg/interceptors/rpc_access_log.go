package interceptors

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/sonic-net/sonic-gnmi/pkg/logging"
)

const (
	rpcAccessLogPrefix        = "RPC_ACCESS"
	rpcAccessLogSummaryPrefix = "RPC_ACCESS_SUMMARY"
	rpcAccessLogInterval      = 10 * time.Second

	// rpcKeyedLimiterMaxKeys caps the key-state map to prevent unbounded growth.
	rpcKeyedLimiterMaxKeys = 10_000
)

type logfFunc func(string, ...interface{})
type scheduleFunc func(time.Duration, func())

type rpcLogKey struct {
	method string
	code   codes.Code
}

type rpcLogLimit struct {
	limiter    *rate.Limiter
	suppressed uint64
	scheduled  bool
}

// rpcKeyedLimiter is a configurable per-(method,code) rate limiter with
// zero/off semantics, bounded key state, and an injectable clock.
//
// PROHIBITION (RD-032): This limiter and any derived configuration MUST NOT be
// applied to GNMI_AUDIT events or any security-required event class. Doing so
// would violate NFR-001 (GNMI_AUDIT events must be unsampled) and FR-003 (one
// record per handler invocation). Any change requires a formal
// security-requirements update in ADO #39044522 and explicit
// security-owner approval.
type rpcKeyedLimiter struct {
	interval time.Duration
	burst    int
	now      func() time.Time
	maxKeys  int
	mu       sync.Mutex
	limits   map[rpcLogKey]*rpcLogLimit
}

// allow reports whether the event for key should be logged.
//
// Returns (suppressed, allowed, scheduleNeeded):
//   - suppressed: count of events suppressed since the last allowed event
//   - allowed: whether this event should be logged
//   - scheduleNeeded: caller must schedule a summary flush if true
//
// When interval<=0 or burst<=0 (disabled), all events are allowed without
// creating any key state (zero/off semantics).
// When the map is at maxKeys capacity and the key is new, the event is allowed
// without tracking (cap fallback).
func (lim *rpcKeyedLimiter) allow(key rpcLogKey) (suppressed uint64, allowed bool, scheduleNeeded bool) {
	if lim.interval <= 0 || lim.burst <= 0 {
		return 0, true, false
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()

	limit, ok := lim.limits[key]
	if !ok {
		if len(lim.limits) >= lim.maxKeys {
			return 0, true, false
		}
		limit = &rpcLogLimit{
			limiter: rate.NewLimiter(rate.Every(lim.interval), lim.burst),
		}
		lim.limits[key] = limit
	}
	if !limit.limiter.AllowN(lim.now(), 1) {
		limit.suppressed++
		needSchedule := !limit.scheduled
		if needSchedule {
			limit.scheduled = true
		}
		return 0, false, needSchedule
	}
	suppressed = limit.suppressed
	limit.suppressed = 0
	return suppressed, true, false
}

// flushSummary attempts to emit a suppression summary for key.
//
// Returns (suppressed, emitted, reschedule):
//   - suppressed: count of suppressed events (valid only when emitted=true)
//   - emitted: true if a summary record should be written
//   - reschedule: true if the caller must reschedule the summary flush
//
// If the key is absent or suppressed count is zero, all return values are zero.
func (lim *rpcKeyedLimiter) flushSummary(key rpcLogKey) (suppressed uint64, emitted bool, reschedule bool) {
	lim.mu.Lock()
	defer lim.mu.Unlock()

	limit, ok := lim.limits[key]
	if !ok {
		return 0, false, false
	}
	limit.scheduled = false
	if limit.suppressed == 0 {
		return 0, false, false
	}
	if !limit.limiter.AllowN(lim.now(), 1) {
		limit.scheduled = true
		return 0, false, true
	}
	suppressed = limit.suppressed
	limit.suppressed = 0
	return suppressed, true, false
}

// rpcLogger emits at most one access record or suppression summary per method
// and status-code pair during each interval.
type rpcLogger struct {
	logf     logfFunc
	now      func() time.Time
	schedule scheduleFunc
	limiter  *rpcKeyedLimiter
}

type rpcAccessLogRecord struct {
	Version    int    `json:"v"`
	RPCType    string `json:"type"`
	Method     string `json:"method"`
	PeerType   string `json:"peer_type"`
	Peer       string `json:"peer"`
	Code       string `json:"code"`
	DurationMS int64  `json:"duration_ms"`
	Suppressed uint64 `json:"suppressed"`
}

type rpcAccessLogSummary struct {
	Version    int    `json:"v"`
	Method     string `json:"method"`
	Code       string `json:"code"`
	Suppressed uint64 `json:"suppressed"`
}

func newRPCLogger(logf logfFunc) *rpcLogger {
	return newRPCLoggerWithClock(logf, time.Now, func(delay time.Duration, f func()) {
		time.AfterFunc(delay, f)
	})
}

func newRPCLoggerWithClock(logf logfFunc, now func() time.Time, schedule scheduleFunc) *rpcLogger {
	return &rpcLogger{
		logf:     logf,
		now:      now,
		schedule: schedule,
		limiter: &rpcKeyedLimiter{
			interval: rpcAccessLogInterval,
			burst:    1,
			now:      now,
			maxKeys:  rpcKeyedLimiterMaxKeys,
			limits:   make(map[rpcLogKey]*rpcLogLimit),
		},
	}
}

func (l *rpcLogger) log(ctx context.Context, rpcType, method string, started time.Time, err error) {
	finished := l.now()
	code := logging.GRPCCode(err)
	suppressed, allowed := l.allow(method, code)
	if !allowed {
		return
	}

	peerType, peerAddress := logging.PeerTypeAddr(ctx)
	sink := func(line string) error { l.logf("%s", line); return nil }
	_ = logging.WriteJSON(rpcAccessLogPrefix, rpcAccessLogRecord{
		Version:    2,
		RPCType:    rpcType,
		Method:     method,
		PeerType:   peerType,
		Peer:       peerAddress,
		Code:       code.String(),
		DurationMS: finished.Sub(started).Milliseconds(),
		Suppressed: suppressed,
	}, sink)
}

func (l *rpcLogger) allow(method string, code codes.Code) (uint64, bool) {
	key := rpcLogKey{method: method, code: code}
	suppressed, allowed, scheduleNeeded := l.limiter.allow(key)
	if scheduleNeeded {
		l.schedule(rpcAccessLogInterval, func() { l.writeSummary(key) })
	}
	return suppressed, allowed
}

func (l *rpcLogger) writeSummary(key rpcLogKey) {
	suppressed, emitted, reschedule := l.limiter.flushSummary(key)
	if reschedule {
		l.schedule(rpcAccessLogInterval, func() { l.writeSummary(key) })
		return
	}
	if !emitted {
		return
	}
	sink := func(line string) error { l.logf("%s", line); return nil }
	_ = logging.WriteJSON(rpcAccessLogSummaryPrefix, rpcAccessLogSummary{
		Version:    1,
		Method:     key.method,
		Code:       key.code.String(),
		Suppressed: suppressed,
	}, sink)
}

// UnaryInterceptor logs the final outcome of a unary RPC.
func (l *rpcLogger) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		started := l.now()
		response, err := handler(ctx, req)
		l.log(ctx, "unary", info.FullMethod, started, err)
		return response, err
	}
}

// StreamInterceptor logs the final outcome of a streaming RPC.
func (l *rpcLogger) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		started := l.now()
		err := handler(srv, stream)
		l.log(stream.Context(), "stream", info.FullMethod, started, err)
		return err
	}
}
