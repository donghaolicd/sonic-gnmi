package gnmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type auditTestWriter struct {
	mu         sync.Mutex
	lines      []string
	infoErr    error
	panicInfo  bool
	infoCalls  int
	closeCalls int
}

func (w *auditTestWriter) Info(line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.infoCalls++
	if w.panicInfo {
		panic("sensitive panic text")
	}
	if w.infoErr != nil {
		return w.infoErr
	}
	w.lines = append(w.lines, line)
	return nil
}

func (w *auditTestWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeCalls++
	return nil
}

func (w *auditTestWriter) snapshot() (lines []string, infoCalls, closeCalls int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...), w.infoCalls, w.closeCalls
}

func auditTestContext(t *testing.T) context.Context {
	t.Helper()
	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.8"), Port: 52144}
	return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
}

func keyedPath(name, keyName, keyValue string, rest ...string) *gnmipb.Path {
	elems := []*gnmipb.PathElem{{
		Name: name,
		Key:  map[string]string{keyName: keyValue},
	}}
	for _, elem := range rest {
		elems = append(elems, &gnmipb.PathElem{Name: elem})
	}
	return &gnmipb.Path{Elem: elems}
}

func decodeAuditLine(t *testing.T, line string) map[string]any {
	t.Helper()
	const prefix = "GNMI_AUDIT "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("audit line %q does not start with %q", line, prefix)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &fields); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	return fields
}

func TestGNMIAuditGetSchemaAndRedaction(t *testing.T) {
	start := time.Date(2026, time.August, 3, 19, 0, 0, 123456789, time.UTC)
	finish := start.Add(8 * time.Millisecond)
	writer := &auditTestWriter{}
	logger := newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil },
		func() time.Time { return finish },
		func() {},
	)

	req := &gnmipb.GetRequest{
		Prefix: &gnmipb.Path{
			Target: "SECRET_TARGET",
			Origin: "SECRET_ORIGIN",
			Elem:   []*gnmipb.PathElem{{Name: "interfaces"}},
		},
		Path: []*gnmipb.Path{
			keyedPath("interface", "name", "Ethernet0", "config"),
			keyedPath("interface", "name", "Ethernet0", "config"),
			{Elem: []*gnmipb.PathElem{{Name: "system"}, {Name: "config"}}},
		},
		Type:     gnmipb.GetRequest_ALL,
		Encoding: gnmipb.Encoding_JSON_IETF,
		UseModels: []*gnmipb.ModelData{{
			Name: "openconfig-interfaces",
		}},
	}
	record := logger.newGetRecord(auditTestContext(t), "TELEMETRY-42", req, start)
	if record.Get.AuthorizedPathCount != 0 {
		t.Fatalf("AuthorizedPathCount before pathz = %d, want 0", record.Get.AuthorizedPathCount)
	}
	record.IdentityState = identityValidated
	record.Principal = "admin"
	record.AccessResult = accessAllowed
	record.PathAuthzResult = getPathAllAllowed
	record.Reason = reasonCompleted
	record.Get.ClientType = clientTranslib
	record.Get.AuthorizedPathCount = 3

	logger.emitGet(record, nil)

	lines, calls, _ := writer.snapshot()
	if calls != 1 || len(lines) != 1 {
		t.Fatalf("writer calls/lines = %d/%d, want 1/1", calls, len(lines))
	}
	if strings.Contains(lines[0], "Ethernet0") ||
		strings.Contains(lines[0], "name") ||
		strings.Contains(lines[0], "SECRET_TARGET") ||
		strings.Contains(lines[0], "SECRET_ORIGIN") {
		t.Fatalf("audit line leaked path key, target, or origin: %s", lines[0])
	}

	fields := decodeAuditLine(t, lines[0])
	wantFields := []string{
		"access_result", "duration_ms", "get", "grpc_code", "identity_state",
		"method", "path_authz_result", "path_count", "paths", "paths_truncated",
		"peer", "peer_type", "principal", "reason", "request_id", "time", "v",
	}
	if got := sortedMapKeys(fields); fmt.Sprint(got) != fmt.Sprint(wantFields) {
		t.Fatalf("fields = %v, want %v", got, wantFields)
	}
	if fields["method"] != "get" || fields["grpc_code"] != codes.OK.String() {
		t.Fatalf("method/code = %v/%v, want get/OK", fields["method"], fields["grpc_code"])
	}
	if fields["time"] != start.Format(time.RFC3339Nano) || fields["duration_ms"] != float64(8) {
		t.Fatalf("time/duration = %v/%v", fields["time"], fields["duration_ms"])
	}
	if fields["path_count"] != float64(3) || fields["paths_truncated"] != float64(0) {
		t.Fatalf("path count/truncated = %v/%v", fields["path_count"], fields["paths_truncated"])
	}
	paths := fields["paths"].([]any)
	if got := fmt.Sprint(paths); got != "[/interfaces/interface/config /interfaces/system/config]" {
		t.Fatalf("paths = %s", got)
	}
	getFields := fields["get"].(map[string]any)
	if _, ok := fields["set"]; ok {
		t.Fatal("Get audit unexpectedly contains set object")
	}
	if getFields["request_type"] != "all" ||
		getFields["encoding"] != "JSON_IETF" ||
		getFields["model_count"] != float64(1) ||
		getFields["client_type"] != "translib" ||
		getFields["authorized_path_count"] != float64(3) {
		t.Fatalf("get fields = %#v", getFields)
	}
}

func TestGNMIAuditSetSchemaOmitsValues(t *testing.T) {
	start := time.Date(2026, time.August, 3, 19, 0, 0, 0, time.UTC)
	writer := &auditTestWriter{}
	logger := newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil },
		func() time.Time { return start.Add(15 * time.Millisecond) },
		func() {},
	)
	req := &gnmipb.SetRequest{
		Prefix: &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: "interfaces"}}},
		Update: []*gnmipb.Update{{
			Path: keyedPath("interface", "name", "Ethernet0", "config"),
			Val: &gnmipb.TypedValue{
				Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(`{"password":"DO_NOT_LOG"}`)},
			},
		}},
	}
	record := logger.newSetRecord(auditTestContext(t), "TELEMETRY-43", req, start)
	record.IdentityState = identityValidated
	record.Principal = "admin"
	record.AccessResult = accessAllowed
	record.PathAuthzResult = setPathAllowed
	record.Reason = reasonCompleted
	record.Set.Backend = backendTranslib
	record.Set.ExecutionResult = executionSucceeded

	logger.emitSet(record, nil)

	lines, calls, _ := writer.snapshot()
	if calls != 1 || len(lines) != 1 {
		t.Fatalf("writer calls/lines = %d/%d, want 1/1", calls, len(lines))
	}
	if strings.Contains(lines[0], "DO_NOT_LOG") ||
		strings.Contains(lines[0], "password") ||
		strings.Contains(lines[0], "Ethernet0") ||
		strings.Contains(lines[0], "name") {
		t.Fatalf("audit line leaked request value or path key: %s", lines[0])
	}
	fields := decodeAuditLine(t, lines[0])
	if fields["method"] != "set" || fields["duration_ms"] != float64(15) {
		t.Fatalf("method/duration = %v/%v", fields["method"], fields["duration_ms"])
	}
	if _, ok := fields["get"]; ok {
		t.Fatal("Set audit unexpectedly contains get object")
	}
	setFields := fields["set"].(map[string]any)
	if setFields["delete_count"] != float64(0) ||
		setFields["replace_count"] != float64(0) ||
		setFields["update_count"] != float64(1) ||
		setFields["backend"] != "translib" ||
		setFields["execution_result"] != "succeeded" {
		t.Fatalf("set fields = %#v", setFields)
	}
}

func TestGNMIAuditPathCapAndTruncation(t *testing.T) {
	paths := make([]*gnmipb.Path, 0, auditPathLimit+3)
	for i := 0; i < auditPathLimit+3; i++ {
		paths = append(paths, &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: fmt.Sprintf("path-%02d", i)}}})
	}
	req := &gnmipb.GetRequest{Path: paths}
	logger := newGNMIAuditLogger(nil, time.Now, func() {})
	record := logger.newGetRecord(auditTestContext(t), "TELEMETRY-CAP", req, time.Now())

	if record.PathCount != auditPathLimit+3 {
		t.Fatalf("PathCount = %d, want %d", record.PathCount, auditPathLimit+3)
	}
	if len(record.Paths) != auditPathLimit || record.PathsTruncated != 3 {
		t.Fatalf("paths/truncated = %d/%d, want %d/3", len(record.Paths), record.PathsTruncated, auditPathLimit)
	}
}

func TestGNMIAuditDatabasePathRedaction(t *testing.T) {
	tests := []struct {
		name   string
		prefix *gnmipb.Path
		path   *gnmipb.Path
		want   string
	}{
		{
			name:   "direct DB omits positional key and field",
			prefix: &gnmipb.Path{Target: "CONFIG_DB"},
			path: &gnmipb.Path{Elem: []*gnmipb.PathElem{
				{Name: "DEVICE_METADATA"}, {Name: "SECRET_KEY"}, {Name: "SECRET_FIELD"},
			}},
			want: "/DEVICE_METADATA",
		},
		{
			name:   "native DB keeps database and table only",
			prefix: &gnmipb.Path{Origin: "sonic-db"},
			path: &gnmipb.Path{Elem: []*gnmipb.PathElem{
				{Name: "CONFIG_DB"}, {Name: "SECRET_INSTANCE"}, {Name: "PORT"}, {Name: "SECRET_KEY"},
			}},
			want: "/CONFIG_DB/PORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAuditPath(tc.prefix, tc.path)
			if got != tc.want {
				t.Fatalf("redactAuditPath() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Fatalf("redacted path leaked positional key: %q", got)
			}
		})
	}
}

func TestGNMIAuditLoggerFailureAndReconnect(t *testing.T) {
	t.Run("marshal failure is counted without calling sink", func(t *testing.T) {
		writer := &auditTestWriter{}
		var losses atomic.Int64
		logger := newGNMIAuditLogger(
			func() (auditSyslogWriter, error) { return writer, nil },
			time.Now,
			func() { losses.Add(1) },
		)
		logger.emit(make(chan int))
		_, calls, closes := writer.snapshot()
		if calls != 0 || closes != 1 || losses.Load() != 1 {
			t.Fatalf("calls/closes/loss = %d/%d/%d", calls, closes, losses.Load())
		}
	})

	t.Run("write failure is not retried and next event reconnects", func(t *testing.T) {
		first := &auditTestWriter{infoErr: errors.New("write failed")}
		second := &auditTestWriter{}
		var connectCalls int
		var losses atomic.Int64
		logger := newGNMIAuditLogger(
			func() (auditSyslogWriter, error) {
				connectCalls++
				if connectCalls == 1 {
					return first, nil
				}
				return second, nil
			},
			time.Now,
			func() { losses.Add(1) },
		)

		logger.emitGet(minimalGetRecord(time.Now()), nil)
		_, firstCalls, firstCloses := first.snapshot()
		if firstCalls != 1 || firstCloses != 1 || connectCalls != 1 || losses.Load() != 1 {
			t.Fatalf("first calls/closes/connect/loss = %d/%d/%d/%d", firstCalls, firstCloses, connectCalls, losses.Load())
		}

		logger.emitGet(minimalGetRecord(time.Now()), nil)
		lines, secondCalls, _ := second.snapshot()
		if secondCalls != 1 || len(lines) != 1 || connectCalls != 2 || losses.Load() != 1 {
			t.Fatalf("second calls/lines/connect/loss = %d/%d/%d/%d", secondCalls, len(lines), connectCalls, losses.Load())
		}
	})

	t.Run("connect failure is counted and next event reconnects", func(t *testing.T) {
		writer := &auditTestWriter{}
		var connectCalls int
		var losses atomic.Int64
		logger := newGNMIAuditLogger(
			func() (auditSyslogWriter, error) {
				connectCalls++
				if connectCalls == 1 {
					return nil, errors.New("connect failed")
				}
				return writer, nil
			},
			time.Now,
			func() { losses.Add(1) },
		)
		logger.emitSet(minimalSetRecord(time.Now()), nil)
		logger.emitSet(minimalSetRecord(time.Now()), nil)
		lines, calls, _ := writer.snapshot()
		if connectCalls != 2 || calls != 1 || len(lines) != 1 || losses.Load() != 1 {
			t.Fatalf("connect/calls/lines/loss = %d/%d/%d/%d", connectCalls, calls, len(lines), losses.Load())
		}
	})

	t.Run("sink panic is counted without leaking panic text", func(t *testing.T) {
		writer := &auditTestWriter{panicInfo: true}
		var losses atomic.Int64
		logger := newGNMIAuditLogger(
			func() (auditSyslogWriter, error) { return writer, nil },
			time.Now,
			func() { losses.Add(1) },
		)
		logger.emitGet(minimalGetRecord(time.Now()), nil)
		_, calls, closes := writer.snapshot()
		if calls != 1 || closes != 1 || losses.Load() != 1 {
			t.Fatalf("calls/closes/loss = %d/%d/%d", calls, closes, losses.Load())
		}
	})
}

func TestGNMIAuditLoggerConcurrent(t *testing.T) {
	writer := &auditTestWriter{}
	logger := newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil },
		time.Now,
		func() {},
	)
	const count = 100
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				logger.emitGet(minimalGetRecord(time.Now()), nil)
			} else {
				logger.emitSet(minimalSetRecord(time.Now()), status.Error(codes.Internal, "not logged"))
			}
		}(i)
	}
	wg.Wait()
	lines, calls, _ := writer.snapshot()
	if calls != count || len(lines) != count {
		t.Fatalf("calls/lines = %d/%d, want %d/%d", calls, len(lines), count, count)
	}
}

func TestGNMIAuditGetConcurrent(t *testing.T) {
	writer := &auditTestWriter{}
	logger := newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil },
		time.Now,
		func() {},
	)
	const count = 50
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			logger.emitGet(minimalGetRecord(time.Now()), nil)
		}()
	}
	wg.Wait()
	lines, calls, _ := writer.snapshot()
	if calls != count || len(lines) != count {
		t.Fatalf("calls/lines = %d/%d, want %d/%d", calls, len(lines), count, count)
	}
	for _, line := range lines {
		if method := decodeAuditLine(t, line)["method"]; method != "get" {
			t.Fatalf("method = %v, want get", method)
		}
	}
}

func TestGNMIAuditPanicFinalizers(t *testing.T) {
	tests := []struct {
		name         string
		invoke       func(*gnmiAuditLogger)
		wantMethod   string
		wantPanicked bool
	}{
		{
			name: "get",
			invoke: func(logger *gnmiAuditLogger) {
				record := minimalGetRecord(time.Now())
				var rpcErr error
				defer logger.finishGet(record, &rpcErr)
				panic("GET_SECRET_PANIC")
			},
			wantMethod: "get",
		},
		{
			name: "set",
			invoke: func(logger *gnmiAuditLogger) {
				record := minimalSetRecord(time.Now())
				var rpcErr error
				defer logger.finishSet(record, &rpcErr)
				panic("SET_SECRET_PANIC")
			},
			wantMethod:   "set",
			wantPanicked: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writer := &auditTestWriter{}
			logger := newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil },
				time.Now,
				func() {},
			)
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				tc.invoke(logger)
			}()
			if recovered == nil {
				t.Fatal("handler panic was not re-propagated")
			}
			lines, calls, _ := writer.snapshot()
			if calls != 1 || len(lines) != 1 {
				t.Fatalf("calls/lines = %d/%d, want 1/1", calls, len(lines))
			}
			if strings.Contains(lines[0], "SECRET_PANIC") {
				t.Fatalf("audit line leaked panic value: %s", lines[0])
			}
			fields := decodeAuditLine(t, lines[0])
			if fields["method"] != tc.wantMethod ||
				fields["reason"] != "handler_panic" ||
				fields["grpc_code"] != codes.Unknown.String() {
				t.Fatalf("panic fields = %#v", fields)
			}
			if tc.wantPanicked {
				setFields := fields["set"].(map[string]any)
				if setFields["execution_result"] != "panicked" {
					t.Fatalf("execution_result = %v, want panicked", setFields["execution_result"])
				}
			}
		})
	}
}

func TestGNMIAuditFiniteTokens(t *testing.T) {
	tokens := []fmt.Stringer{
		identityValidated,
		accessAllowed,
		getPathAllAllowed,
		setPathAllowed,
		reasonCompleted,
		clientTranslib,
		backendTranslib,
		executionSucceeded,
	}
	want := []string{"validated", "allowed", "all_allowed", "allowed", "completed", "translib", "translib", "succeeded"}
	for i, token := range tokens {
		if token.String() != want[i] {
			t.Fatalf("token %d = %q, want %q", i, token.String(), want[i])
		}
	}
}

func TestGNMIAuditStageAccess(t *testing.T) {
	tests := []struct {
		name          string
		authEnabled   bool
		username      string
		transportUser string
		peerType      string
		authErr       error
		wantIdentity  identityState
		wantPrincipal string
		wantAccess    accessResult
	}{
		{
			name:          "validated",
			authEnabled:   true,
			username:      "admin",
			peerType:      "tcp",
			wantIdentity:  identityValidated,
			wantPrincipal: "admin",
			wantAccess:    accessAllowed,
		},
		{
			name:          "authorized identity denied",
			authEnabled:   true,
			username:      "admin",
			peerType:      "tcp",
			authErr:       status.Error(codes.PermissionDenied, "role denied"),
			transportUser: "admin",
			wantIdentity:  identityTransportValidated,
			wantPrincipal: "admin",
			wantAccess:    accessDenied,
		},
		{
			name:         "authentication failed",
			authEnabled:  true,
			username:     "caller-controlled",
			peerType:     "tcp",
			authErr:      status.Error(codes.Unauthenticated, "failed"),
			wantIdentity: identityNotValidated,
			wantAccess:   accessDenied,
		},
		{
			name:         "auth disabled",
			peerType:     "tcp",
			wantIdentity: identityAuthDisabled,
			wantAccess:   accessAuthDisabled,
		},
		{
			name:         "local UDS",
			peerType:     "unix",
			wantIdentity: identityLocalUDS,
			wantAccess:   accessLocalUDS,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			common := &gnmiAuditCommon{}
			common.stageAccess(tc.authEnabled, tc.username, tc.transportUser, tc.peerType, tc.authErr)
			if common.IdentityState != tc.wantIdentity ||
				common.Principal != tc.wantPrincipal ||
				common.AccessResult != tc.wantAccess {
				t.Fatalf("stageAccess() = (%q,%q,%q), want (%q,%q,%q)",
					common.IdentityState, common.Principal, common.AccessResult,
					tc.wantIdentity, tc.wantPrincipal, tc.wantAccess)
			}
		})
	}
}

func minimalGetRecord(start time.Time) *gnmiGetAuditRecord {
	return &gnmiGetAuditRecord{
		gnmiAuditCommon: gnmiAuditCommon{
			Version:         gnmiAuditVersion,
			Method:          auditMethodGet,
			Time:            start.Format(time.RFC3339Nano),
			IdentityState:   identityNotEvaluated,
			AccessResult:    accessNotEvaluated,
			PathAuthzResult: getPathNotEvaluated,
			Reason:          reasonCompleted,
			Paths:           []string{},
			started:         start,
		},
		Get: gnmiGetFields{
			RequestType: "all",
			Encoding:    "JSON_IETF",
			ClientType:  clientNone,
		},
	}
}

func minimalSetRecord(start time.Time) *gnmiSetAuditRecord {
	return &gnmiSetAuditRecord{
		gnmiAuditCommon: gnmiAuditCommon{
			Version:         gnmiAuditVersion,
			Method:          auditMethodSet,
			Time:            start.Format(time.RFC3339Nano),
			IdentityState:   identityNotEvaluated,
			AccessResult:    accessNotEvaluated,
			PathAuthzResult: setPathNotEvaluated,
			Reason:          reasonCompleted,
			Paths:           []string{},
			started:         start,
		},
		Set: gnmiSetFields{
			Backend:         backendNone,
			ExecutionResult: executionNotStarted,
		},
	}
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
