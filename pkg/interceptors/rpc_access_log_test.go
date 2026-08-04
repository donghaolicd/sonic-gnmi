package interceptors

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type accessLogRecord struct {
	Version    int    `json:"v"`
	RPCType    string `json:"type"`
	Method     string `json:"method"`
	PeerType   string `json:"peer_type"`
	Peer       string `json:"peer"`
	Code       string `json:"code"`
	DurationMS int64  `json:"duration_ms"`
	Suppressed uint64 `json:"suppressed"`
}

type accessLogSummary struct {
	Version    int    `json:"v"`
	Method     string `json:"method"`
	Code       string `json:"code"`
	Suppressed uint64 `json:"suppressed"`
}

type accessLogServerStream struct {
	ctx context.Context
}

func (s *accessLogServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *accessLogServerStream) SendHeader(metadata.MD) error { return nil }
func (s *accessLogServerStream) SetTrailer(metadata.MD)       {}
func (s *accessLogServerStream) Context() context.Context     { return s.ctx }
func (s *accessLogServerStream) SendMsg(interface{}) error    { return nil }
func (s *accessLogServerStream) RecvMsg(interface{}) error    { return nil }

func captureAccessLog(t *testing.T) (func(string, ...interface{}), func() accessLogRecord) {
	t.Helper()

	var lines []string
	logf := func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	record := func() accessLogRecord {
		t.Helper()
		if len(lines) != 1 {
			t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
		}
		return parseAccessLog(t, lines[0])
	}
	return logf, record
}

func parseAccessLog(t *testing.T, line string) accessLogRecord {
	t.Helper()

	const prefix = "RPC_ACCESS "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("log line %q does not start with %q", line, prefix)
	}

	payload := []byte(strings.TrimPrefix(line, prefix))
	var fields map[string]interface{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	slices.Sort(gotFields)
	wantFields := []string{"code", "duration_ms", "method", "peer", "peer_type", "suppressed", "type", "v"}
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("log record fields = %v, want %v", gotFields, wantFields)
	}

	var got accessLogRecord
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("cannot decode access log: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("access log version = %d, want 2", got.Version)
	}
	return got
}

func parseAccessLogSummary(t *testing.T, line string) accessLogSummary {
	t.Helper()

	const prefix = "RPC_ACCESS_SUMMARY "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("log line %q does not start with %q", line, prefix)
	}
	payload := []byte(strings.TrimPrefix(line, prefix))
	var fields map[string]interface{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("log summary is not valid JSON: %v", err)
	}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	slices.Sort(gotFields)
	wantFields := []string{"code", "method", "suppressed", "v"}
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("log summary fields = %v, want %v", gotFields, wantFields)
	}

	var got accessLogSummary
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("cannot decode access log summary: %v", err)
	}
	return got
}

func TestRPCLoggerEndToEnd(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() failed: %v", err)
	}
	defer listener.Close()

	logs := make(chan string, 1)
	logger := newRPCLogger(func(format string, args ...interface{}) {
		logs <- fmt.Sprintf(format, args...)
	})
	server := grpc.NewServer(
		grpc.UnaryInterceptor(logger.UnaryInterceptor()),
		grpc.StreamInterceptor(logger.StreamInterceptor()),
	)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil {
			select {
			case logs <- fmt.Sprintf("serve error: %v", serveErr):
			default:
			}
		}
	}()
	t.Cleanup(server.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() failed: %v", err)
	}
	defer conn.Close()

	response, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Health.Check() failed: %v", err)
	}
	if response.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("Health.Check() status = %v, want SERVING", response.Status)
	}

	select {
	case line := <-logs:
		got := parseAccessLog(t, line)
		if got.RPCType != "unary" || got.Method != "/grpc.health.v1.Health/Check" ||
			got.PeerType != "tcp" || got.Code != codes.OK.String() {
			t.Fatalf("access log = %+v, want successful TCP Health.Check", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for RPC access log")
	}
}

func TestRPCLoggerRateLimitsEachMethodAndCode(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	var lines []string
	logger := newRPCLoggerWithClock(func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}, func() time.Time { return now }, func(time.Duration, func()) {})
	interceptor := logger.UnaryInterceptor()

	call := func(method string, wantErr error) {
		t.Helper()
		_, err := interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, interface{}) (interface{}, error) { return nil, wantErr })
		if err != wantErr {
			t.Fatalf("UnaryInterceptor() error = %v, want %v", err, wantErr)
		}
	}

	call("/gnmi.gNMI/Get", nil)
	call("/gnmi.gNMI/Get", nil)
	call("/gnmi.gNMI/Get", nil)
	if len(lines) != 1 {
		t.Fatalf("same-key calls produced %d log lines, want 1: %v", len(lines), lines)
	}

	permissionDenied := status.Error(codes.PermissionDenied, "not allowed")
	call("/gnmi.gNMI/Get", permissionDenied)
	call("/gnmi.gNMI/Set", nil)
	if len(lines) != 3 {
		t.Fatalf("distinct-key calls produced %d log lines, want 3: %v", len(lines), lines)
	}

	now = now.Add(10 * time.Second)
	call("/gnmi.gNMI/Get", nil)
	if len(lines) != 4 {
		t.Fatalf("call after interval produced %d log lines, want 4: %v", len(lines), lines)
	}
	got := parseAccessLog(t, lines[3])
	if got.Method != "/gnmi.gNMI/Get" || got.Code != codes.OK.String() || got.Suppressed != 2 {
		t.Fatalf("access log = %+v, want Get/OK with 2 suppressed calls", got)
	}
}

func TestRPCLoggerReportsSuppressionAfterTrafficStops(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	var lines []string
	var scheduled func()
	logger := newRPCLoggerWithClock(func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}, func() time.Time { return now }, func(delay time.Duration, f func()) {
		if delay != 10*time.Second {
			t.Fatalf("summary delay = %v, want 10s", delay)
		}
		scheduled = f
	})
	interceptor := logger.UnaryInterceptor()
	call := func() {
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
			func(context.Context, interface{}) (interface{}, error) { return nil, nil })
	}

	call()
	call()
	call()
	if scheduled == nil {
		t.Fatal("suppression summary was not scheduled")
	}
	now = now.Add(10 * time.Second)
	scheduled()

	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want access and summary: %v", len(lines), lines)
	}
	got := parseAccessLogSummary(t, lines[1])
	if got.Version != 1 || got.Method != "/gnmi.gNMI/Get" ||
		got.Code != codes.OK.String() || got.Suppressed != 2 {
		t.Fatalf("access log summary = %+v, want Get/OK with 2 suppressed calls", got)
	}
	call()
	if len(lines) != 2 {
		t.Fatalf("call after summary produced %d log lines, want shared rate limit", len(lines))
	}
}

func TestRPCLoggerRetriesSummaryWhenAccessRecordWinsInterval(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	var lines []string
	var scheduled []func()
	logger := newRPCLoggerWithClock(func(format string, args ...interface{}) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}, func() time.Time { return now }, func(delay time.Duration, f func()) {
		if delay != 10*time.Second {
			t.Fatalf("summary delay = %v, want 10s", delay)
		}
		scheduled = append(scheduled, f)
	})
	interceptor := logger.UnaryInterceptor()
	call := func() {
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
			func(context.Context, interface{}) (interface{}, error) { return nil, nil })
	}

	call()
	call()
	now = now.Add(10 * time.Second)
	call()
	call()
	scheduled[0]()
	if len(scheduled) != 2 {
		t.Fatalf("scheduled callbacks = %d, want summary retry", len(scheduled))
	}
	if len(lines) != 2 {
		t.Fatalf("got %d log lines before retry, want 2: %v", len(lines), lines)
	}

	now = now.Add(10 * time.Second)
	scheduled[1]()
	if len(lines) != 3 {
		t.Fatalf("got %d log lines after retry, want 3: %v", len(lines), lines)
	}
	if got := parseAccessLogSummary(t, lines[2]); got.Suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 since last access record", got.Suppressed)
	}
}

func TestRPCLoggerRateLimitIsConcurrent(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	var lines []string
	var linesMu sync.Mutex
	logger := newRPCLoggerWithClock(func(format string, args ...interface{}) {
		linesMu.Lock()
		defer linesMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}, func() time.Time { return now }, func(time.Duration, func()) {})
	interceptor := logger.UnaryInterceptor()

	const calls = 100
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			_, _ = interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
				func(context.Context, interface{}) (interface{}, error) { return nil, nil })
		}()
	}
	wg.Wait()

	if len(lines) != 1 {
		t.Fatalf("concurrent calls produced %d log lines, want 1", len(lines))
	}
	now = now.Add(10 * time.Second)
	_, _ = interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
		func(context.Context, interface{}) (interface{}, error) { return nil, nil })
	if got := parseAccessLog(t, lines[1]); got.Suppressed != calls-1 {
		t.Fatalf("suppressed = %d, want %d", got.Suppressed, calls-1)
	}
}

func TestRPCLoggerUnaryError(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)
	info := &grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Set"}
	wantErr := status.Error(codes.PermissionDenied, "not allowed")

	response, err := logger.UnaryInterceptor()(context.Background(), "secret request", info,
		func(context.Context, interface{}) (interface{}, error) {
			return nil, wantErr
		})
	if err != wantErr {
		t.Fatalf("UnaryInterceptor() error = %v, want original error %v", err, wantErr)
	}
	if response != nil {
		t.Fatalf("UnaryInterceptor() response = %v, want nil", response)
	}

	got := capturedRecord()
	if got.Code != codes.PermissionDenied.String() {
		t.Fatalf("access log code = %q, want %q", got.Code, codes.PermissionDenied)
	}
	if got.PeerType != "unknown" || got.Peer != "" {
		t.Fatalf("access log peer = %q/%q, want unknown/empty", got.PeerType, got.Peer)
	}
}

func TestRPCLoggerUnaryContextErrorUsesGRPCCode(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)

	_, err := logger.UnaryInterceptor()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
		func(context.Context, interface{}) (interface{}, error) {
			return nil, context.DeadlineExceeded
		})
	if err != context.DeadlineExceeded {
		t.Fatalf("UnaryInterceptor() error = %v, want original context error", err)
	}
	if got := capturedRecord(); got.Code != codes.DeadlineExceeded.String() {
		t.Fatalf("access log code = %q, want %q", got.Code, codes.DeadlineExceeded)
	}
}

func TestRPCLoggerStreamError(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.UnixAddr{Name: "/var/run/gnmi/gnmi.sock", Net: "unix"},
	})
	stream := &accessLogServerStream{ctx: ctx}
	info := &grpc.StreamServerInfo{FullMethod: "/gnmi.gNMI/Subscribe"}
	wantErr := status.Error(codes.Canceled, "client closed stream")

	err := logger.StreamInterceptor()(nil, stream, info,
		func(interface{}, grpc.ServerStream) error {
			return wantErr
		})
	if err != wantErr {
		t.Fatalf("StreamInterceptor() error = %v, want original error %v", err, wantErr)
	}

	got := capturedRecord()
	want := accessLogRecord{
		Version:  2,
		RPCType:  "stream",
		Method:   "/gnmi.gNMI/Subscribe",
		PeerType: "unix",
		Peer:     "/var/run/gnmi/gnmi.sock",
		Code:     codes.Canceled.String(),
	}
	if got.Version != want.Version || got.RPCType != want.RPCType || got.Method != want.Method ||
		got.PeerType != want.PeerType || got.Peer != want.Peer || got.Code != want.Code {
		t.Fatalf("access log = %+v, want %+v", got, want)
	}
}

func TestRPCLoggerStreamSuccess(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)
	stream := &accessLogServerStream{ctx: context.Background()}

	err := logger.StreamInterceptor()(nil, stream,
		&grpc.StreamServerInfo{FullMethod: "/gnmi.gNMI/Subscribe"},
		func(interface{}, grpc.ServerStream) error { return nil })
	if err != nil {
		t.Fatalf("StreamInterceptor() returned error: %v", err)
	}
	if got := capturedRecord(); got.RPCType != "stream" || got.Code != codes.OK.String() {
		t.Fatalf("access log = %+v, want successful stream", got)
	}
}

func TestRPCLoggerDoesNotPropagateLoggingPanic(t *testing.T) {
	logger := newRPCLogger(func(string, ...interface{}) { panic("log sink failure") })

	response, err := logger.UnaryInterceptor()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"},
		func(context.Context, interface{}) (interface{}, error) { return "response", nil })
	if err != nil || response != "response" {
		t.Fatalf("UnaryInterceptor() = %v, %v; want response, nil", response, err)
	}
}

func TestRPCLoggerRecordsInnerShortCircuit(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)
	calls := []string{}
	shortCircuit := &mockInterceptor{name: "short-circuit", calls: &calls, shouldReplace: true}
	chain := NewChain(logger, shortCircuit)

	response, err := chain.UnaryInterceptor()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/gnoi.os.OS/Activate"},
		func(context.Context, interface{}) (interface{}, error) {
			t.Fatal("handler called after inner interceptor short-circuited")
			return nil, nil
		})
	if err != nil {
		t.Fatalf("UnaryInterceptor() returned error: %v", err)
	}
	if response != "short-circuit response" {
		t.Fatalf("UnaryInterceptor() response = %v, want short-circuit response", response)
	}
	if got := capturedRecord(); got.Method != "/gnoi.os.OS/Activate" || got.Code != codes.OK.String() {
		t.Fatalf("access log = %+v, want short-circuited RPC with OK status", got)
	}
}

func TestRPCLoggerUnarySuccess(t *testing.T) {
	logf, capturedRecord := captureAccessLog(t)
	logger := newRPCLogger(logf)
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 50051},
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/gnmi.gNMI/Get"}

	response, err := logger.UnaryInterceptor()(ctx, "secret request", info,
		func(context.Context, interface{}) (interface{}, error) {
			return "response", nil
		})
	if err != nil {
		t.Fatalf("UnaryInterceptor() returned error: %v", err)
	}
	if response != "response" {
		t.Fatalf("UnaryInterceptor() response = %v, want response", response)
	}

	got := capturedRecord()
	want := accessLogRecord{
		Version:  2,
		RPCType:  "unary",
		Method:   "/gnmi.gNMI/Get",
		PeerType: "tcp",
		Peer:     "192.0.2.10:50051",
		Code:     codes.OK.String(),
	}
	if got.Version != want.Version || got.RPCType != want.RPCType || got.Method != want.Method ||
		got.PeerType != want.PeerType || got.Peer != want.Peer || got.Code != want.Code {
		t.Fatalf("access log = %+v, want %+v", got, want)
	}
	if got.DurationMS < 0 {
		t.Fatalf("duration_ms = %d, want non-negative", got.DurationMS)
	}
}

// — TEST-029 (EPIC-010/rate-limiter-config): rpcKeyedLimiter isolation tests —

// TestRPCKeyedLimiterZeroIntervalAllowsAll verifies that a zero interval
// (disabled) allows all events and creates no key state (TEST-029).
func TestRPCKeyedLimiterZeroIntervalAllowsAll(t *testing.T) {
	lim := &rpcKeyedLimiter{
		interval: 0,
		burst:    1,
		now:      time.Now,
		maxKeys:  100,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}
	for i := range 50 {
		_, allowed, scheduleNeeded := lim.allow(key)
		if !allowed {
			t.Fatalf("call %d: zero-interval limiter suppressed an event", i+1)
		}
		if scheduleNeeded {
			t.Fatalf("call %d: zero-interval limiter requested a summary schedule", i+1)
		}
	}
	if len(lim.limits) != 0 {
		t.Fatalf("zero-interval limiter: map has %d entries, want 0", len(lim.limits))
	}
}

// TestRPCKeyedLimiterZeroBurstAllowsAll verifies that a zero burst (disabled)
// allows all events and creates no key state (TEST-029).
func TestRPCKeyedLimiterZeroBurstAllowsAll(t *testing.T) {
	lim := &rpcKeyedLimiter{
		interval: 10 * time.Second,
		burst:    0,
		now:      time.Now,
		maxKeys:  100,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}
	for i := range 50 {
		_, allowed, scheduleNeeded := lim.allow(key)
		if !allowed {
			t.Fatalf("call %d: zero-burst limiter suppressed an event", i+1)
		}
		if scheduleNeeded {
			t.Fatalf("call %d: zero-burst limiter requested a summary schedule", i+1)
		}
	}
	if len(lim.limits) != 0 {
		t.Fatalf("zero-burst limiter: map has %d entries, want 0", len(lim.limits))
	}
}

// TestRPCKeyedLimiterNonZeroParity verifies that a non-zero default construction
// (10 s interval, burst 1) produces the same rate-limiting behavior as the
// pre-extraction rpcLogger (TEST-029).
func TestRPCKeyedLimiterNonZeroParity(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	lim := &rpcKeyedLimiter{
		interval: rpcAccessLogInterval,
		burst:    1,
		now:      func() time.Time { return now },
		maxKeys:  rpcKeyedLimiterMaxKeys,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}

	_, allowed, _ := lim.allow(key)
	if !allowed {
		t.Fatal("first call should be allowed")
	}
	for i := range 3 {
		_, allowed, _ := lim.allow(key)
		if allowed {
			t.Fatalf("call %d within interval should be suppressed", i+2)
		}
	}

	now = now.Add(rpcAccessLogInterval)
	suppressed, allowed, _ := lim.allow(key)
	if !allowed {
		t.Fatal("call after interval should be allowed")
	}
	if suppressed != 3 {
		t.Fatalf("suppressed = %d, want 3", suppressed)
	}

	other := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.PermissionDenied}
	_, allowed, _ = lim.allow(other)
	if !allowed {
		t.Fatal("different method+code pair should be tracked independently")
	}
}

// TestRPCKeyedLimiterConcurrentRaceFree verifies that concurrent callers are
// race-free and that exactly burst=1 events are allowed at the same instant
// (TEST-029).
func TestRPCKeyedLimiterConcurrentRaceFree(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	var countMu sync.Mutex
	var allowedCount int
	lim := &rpcKeyedLimiter{
		interval: rpcAccessLogInterval,
		burst:    1,
		now:      func() time.Time { return now },
		maxKeys:  rpcKeyedLimiterMaxKeys,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_, ok, _ := lim.allow(key)
			if ok {
				countMu.Lock()
				allowedCount++
				countMu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != 1 {
		t.Fatalf("concurrent callers: %d allowed, want exactly 1 (burst=1)", allowedCount)
	}
}

// TestRPCKeyedLimiterBoundedMaxKeys verifies that the key-state map is capped
// at maxKeys and that events for over-cap keys are allowed without tracking
// (TEST-029 bounded key state).
func TestRPCKeyedLimiterBoundedMaxKeys(t *testing.T) {
	const keyCap = 5
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	lim := &rpcKeyedLimiter{
		interval: rpcAccessLogInterval,
		burst:    1,
		now:      func() time.Time { return now },
		maxKeys:  keyCap,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}

	for i := range keyCap {
		key := rpcLogKey{method: fmt.Sprintf("/m%d", i), code: codes.OK}
		_, allowed, _ := lim.allow(key)
		if !allowed {
			t.Fatalf("key %d: want allowed, got suppressed", i)
		}
	}
	if len(lim.limits) != keyCap {
		t.Fatalf("map size = %d, want %d after filling to cap", len(lim.limits), keyCap)
	}

	// Keys beyond capacity are allowed without being tracked.
	for i := keyCap; i < keyCap+10; i++ {
		key := rpcLogKey{method: fmt.Sprintf("/m%d", i), code: codes.OK}
		_, allowed, _ := lim.allow(key)
		if !allowed {
			t.Fatalf("over-cap key %d: want allowed (no-track fallback), got suppressed", i)
		}
	}
	if len(lim.limits) != keyCap {
		t.Fatalf("map grew past cap: size = %d, want %d", len(lim.limits), keyCap)
	}
}

// — TEST-030 (EPIC-010/clock-injection): deterministic clock tests —

// TestRPCKeyedLimiterClockAdvance verifies deterministic clock-advance:
// suppressed count accumulates correctly, advancing past the interval allows
// the event and returns the accumulated count (TEST-030).
func TestRPCKeyedLimiterClockAdvance(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	lim := &rpcKeyedLimiter{
		interval: rpcAccessLogInterval,
		burst:    1,
		now:      func() time.Time { return now },
		maxKeys:  rpcKeyedLimiterMaxKeys,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}

	suppressed, allowed, needSchedule := lim.allow(key)
	if !allowed || suppressed != 0 || needSchedule {
		t.Fatalf("first allow = (%d, %v, %v), want (0, true, false)", suppressed, allowed, needSchedule)
	}

	const suppressCount = 5
	for i := range suppressCount {
		suppressed, allowed, needSchedule := lim.allow(key)
		if allowed || suppressed != 0 {
			t.Fatalf("suppressed call %d: allow = (%d, %v), want (0, false)", i+1, suppressed, allowed)
		}
		if i == 0 && !needSchedule {
			t.Fatal("first suppressed call: want scheduleNeeded=true")
		}
		if i > 0 && needSchedule {
			t.Fatalf("subsequent suppressed call %d: want scheduleNeeded=false", i+1)
		}
	}

	now = now.Add(rpcAccessLogInterval)
	suppressed, allowed, needSchedule = lim.allow(key)
	if !allowed {
		t.Fatal("call after interval should be allowed")
	}
	if suppressed != suppressCount {
		t.Fatalf("suppressed = %d, want %d", suppressed, suppressCount)
	}
	if needSchedule {
		t.Fatal("allowed call should not request a schedule")
	}
}

// TestRPCKeyedLimiterFlushSummary verifies flushSummary behavior via injected
// clock: reschedules when the interval has not elapsed, emits when it has, and
// is a no-op when suppressed count is already zero (TEST-030).
func TestRPCKeyedLimiterFlushSummary(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	lim := &rpcKeyedLimiter{
		interval: rpcAccessLogInterval,
		burst:    1,
		now:      func() time.Time { return now },
		maxKeys:  rpcKeyedLimiterMaxKeys,
		limits:   make(map[rpcLogKey]*rpcLogLimit),
	}
	key := rpcLogKey{method: "/gnmi.gNMI/Get", code: codes.OK}

	// No key in map → no-op.
	suppressed, emitted, reschedule := lim.flushSummary(key)
	if suppressed != 0 || emitted || reschedule {
		t.Fatalf("flushSummary on absent key = (%d, %v, %v), want (0, false, false)", suppressed, emitted, reschedule)
	}

	// Allow one event (token consumed), suppress one more.
	lim.allow(key) // allowed
	lim.allow(key) // suppressed; suppressed=1, scheduled=true

	// Flush before interval: token unavailable → reschedule.
	suppressed, emitted, reschedule = lim.flushSummary(key)
	if suppressed != 0 || emitted || !reschedule {
		t.Fatalf("flushSummary before interval = (%d, %v, %v), want (0, false, true)", suppressed, emitted, reschedule)
	}

	// Advance past interval; flush should emit the summary.
	now = now.Add(rpcAccessLogInterval)
	suppressed, emitted, reschedule = lim.flushSummary(key)
	if !emitted || reschedule {
		t.Fatalf("flushSummary after interval: emitted=%v reschedule=%v, want (true, false)", emitted, reschedule)
	}
	if suppressed != 1 {
		t.Fatalf("flushSummary suppressed = %d, want 1", suppressed)
	}

	// A second flush finds suppressed=0 → no-op.
	suppressed, emitted, reschedule = lim.flushSummary(key)
	if suppressed != 0 || emitted || reschedule {
		t.Fatalf("second flushSummary = (%d, %v, %v), want (0, false, false)", suppressed, emitted, reschedule)
	}
}
