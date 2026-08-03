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
)

type logfFunc func(string, ...interface{})
type scheduleFunc func(time.Duration, func())

// rpcLogger emits at most one access record or suppression summary per method
// and status-code pair during each interval.
type rpcLogger struct {
	logf     logfFunc
	now      func() time.Time
	schedule scheduleFunc
	mu       sync.Mutex
	limits   map[rpcLogKey]*rpcLogLimit
}

type rpcLogKey struct {
	method string
	code   codes.Code
}

type rpcLogLimit struct {
	limiter    *rate.Limiter
	suppressed uint64
	scheduled  bool
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
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
}

func (l *rpcLogger) log(ctx context.Context, rpcType, method string, started time.Time, err error) {
	finished := l.now()
	code := logging.GRPCCode(err)
	suppressed, allowed := l.allow(method, code, finished)
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

func (l *rpcLogger) allow(method string, code codes.Code, now time.Time) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := rpcLogKey{method: method, code: code}
	limit, ok := l.limits[key]
	if !ok {
		limit = &rpcLogLimit{
			limiter: rate.NewLimiter(rate.Every(rpcAccessLogInterval), 1),
		}
		l.limits[key] = limit
	}
	if !limit.limiter.AllowN(now, 1) {
		limit.suppressed++
		l.scheduleSummary(key, limit)
		return 0, false
	}
	suppressed := limit.suppressed
	limit.suppressed = 0
	return suppressed, true
}

func (l *rpcLogger) scheduleSummary(key rpcLogKey, limit *rpcLogLimit) {
	if limit.scheduled {
		return
	}
	limit.scheduled = true
	l.schedule(rpcAccessLogInterval, func() { l.writeSummary(key) })
}

func (l *rpcLogger) writeSummary(key rpcLogKey) {
	l.mu.Lock()
	limit := l.limits[key]
	limit.scheduled = false
	if limit.suppressed == 0 {
		l.mu.Unlock()
		return
	}
	if !limit.limiter.AllowN(l.now(), 1) {
		l.scheduleSummary(key, limit)
		l.mu.Unlock()
		return
	}
	suppressed := limit.suppressed
	limit.suppressed = 0
	l.mu.Unlock()

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
