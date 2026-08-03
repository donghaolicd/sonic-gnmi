package gnmi

import (
	"context"
	"errors"
	"log/syslog"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	sharedlog "github.com/sonic-net/sonic-gnmi/pkg/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	gnmiAuditVersion = 1
	gnmiAuditPrefix  = "GNMI_AUDIT"
	auditPathLimit   = 32
)

type auditMethod string

const (
	auditMethodGet auditMethod = "get"
	auditMethodSet auditMethod = "set"
)

type identityState string

const (
	identityValidated          identityState = "validated"
	identityTransportValidated identityState = "transport_validated"
	identityNotEvaluated       identityState = "not_evaluated"
	identityNotValidated       identityState = "not_validated"
	identityAuthDisabled       identityState = "auth_disabled"
	identityLocalUDS           identityState = "local_uds"
)

func (v identityState) String() string { return string(v) }

type accessResult string

const (
	accessAllowed      accessResult = "allowed"
	accessDenied       accessResult = "denied"
	accessNotEvaluated accessResult = "not_evaluated"
	accessAuthDisabled accessResult = "auth_disabled"
	accessLocalUDS     accessResult = "local_uds"
)

func (v accessResult) String() string { return string(v) }

type pathAuthzResult string

const (
	getPathAllAllowed   pathAuthzResult = "all_allowed"
	getPathPartial      pathAuthzResult = "partial_allowed"
	getPathAllDenied    pathAuthzResult = "all_denied"
	getPathError        pathAuthzResult = "error"
	getPathNotEnabled   pathAuthzResult = "not_enabled"
	getPathNotEvaluated pathAuthzResult = "not_evaluated"
	setPathAllowed      pathAuthzResult = "allowed"
	setPathDenied       pathAuthzResult = "denied"
	setPathError        pathAuthzResult = "error"
	setPathNotEnabled   pathAuthzResult = "not_enabled"
	setPathNotEvaluated pathAuthzResult = "not_evaluated"
)

func (v pathAuthzResult) String() string { return string(v) }

type auditReason string

const (
	reasonCompleted          auditReason = "completed"
	reasonClientCreateFailed auditReason = "client_create_failed"
	reasonAccessDenied       auditReason = "access_denied"
	reasonPathPolicyDenied   auditReason = "path_policy_denied"
	reasonPathPolicyError    auditReason = "path_policy_error"
	reasonBackendFailed      auditReason = "backend_failed"
	reasonHandlerPanic       auditReason = "handler_panic"
	reasonUnsupportedType    auditReason = "unsupported_type"
	reasonEncodingModelError auditReason = "encoding_model_error"
	reasonNotMaster          auditReason = "not_master"
	reasonReadOnly           auditReason = "read_only"
	reasonInvalidOrigin      auditReason = "invalid_origin"
	reasonNativeDisabled     auditReason = "native_disabled"
	reasonTranslibDisabled   auditReason = "translib_disabled"
)

func (v auditReason) String() string { return string(v) }

type clientType string

const (
	clientOperational clientType = "operational"
	clientNonDB       clientType = "non_db"
	clientShow        clientType = "show"
	clientDB          clientType = "db"
	clientNative      clientType = "native"
	clientTranslib    clientType = "translib"
	clientNone        clientType = "none"
)

func (v clientType) String() string { return string(v) }

type auditBackend string

const (
	backendNone     auditBackend = "none"
	backendBypass   auditBackend = "bypass"
	backendNative   auditBackend = "native"
	backendTranslib auditBackend = "translib"
)

func (v auditBackend) String() string { return string(v) }

type executionResult string

const (
	executionNotStarted executionResult = "not_started"
	executionSucceeded  executionResult = "succeeded"
	executionFailed     executionResult = "failed"
	executionPanicked   executionResult = "panicked"
)

func (v executionResult) String() string { return string(v) }

type gnmiAuditCommon struct {
	Version         int             `json:"v"`
	Method          auditMethod     `json:"method"`
	Time            string          `json:"time"`
	RequestID       string          `json:"request_id"`
	PeerType        string          `json:"peer_type"`
	Peer            string          `json:"peer"`
	IdentityState   identityState   `json:"identity_state"`
	Principal       string          `json:"principal"`
	AccessResult    accessResult    `json:"access_result"`
	PathAuthzResult pathAuthzResult `json:"path_authz_result"`
	Reason          auditReason     `json:"reason"`
	GRPCCode        string          `json:"grpc_code"`
	PathCount       int             `json:"path_count"`
	Paths           []string        `json:"paths"`
	PathsTruncated  int             `json:"paths_truncated"`
	DurationMS      int64           `json:"duration_ms"`
	started         time.Time
}

type gnmiGetFields struct {
	RequestType         string     `json:"request_type"`
	Encoding            string     `json:"encoding"`
	ModelCount          int        `json:"model_count"`
	ClientType          clientType `json:"client_type"`
	AuthorizedPathCount int        `json:"authorized_path_count"`
}

type gnmiGetAuditRecord struct {
	gnmiAuditCommon
	Get gnmiGetFields `json:"get"`
}

type gnmiSetFields struct {
	DeleteCount     int             `json:"delete_count"`
	ReplaceCount    int             `json:"replace_count"`
	UpdateCount     int             `json:"update_count"`
	Backend         auditBackend    `json:"backend"`
	ExecutionResult executionResult `json:"execution_result"`
}

type gnmiSetAuditRecord struct {
	gnmiAuditCommon
	Set gnmiSetFields `json:"set"`
}

type auditSyslogWriter interface {
	Info(string) error
	Close() error
}

type auditSyslogConnect func() (auditSyslogWriter, error)

type gnmiAuditLogger struct {
	mu      sync.Mutex
	writer  auditSyslogWriter
	connect auditSyslogConnect
	now     func() time.Time
	lost    func()
}

func newGNMIAuditLogger(connect auditSyslogConnect, now func() time.Time, lost func()) *gnmiAuditLogger {
	if now == nil {
		now = time.Now
	}
	if lost == nil {
		lost = func() {}
	}
	return &gnmiAuditLogger{
		connect: connect,
		now:     now,
		lost:    lost,
	}
}

func newProductionGNMIAuditLogger(lost func()) *gnmiAuditLogger {
	return newGNMIAuditLogger(
		func() (auditSyslogWriter, error) {
			return syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_INFO, "gnmi-audit")
		},
		time.Now,
		lost,
	)
}

func (l *gnmiAuditLogger) newGetRecord(ctx context.Context, requestID string, req *gnmipb.GetRequest, started time.Time) *gnmiGetAuditRecord {
	paths, truncated := auditPaths(req.GetPrefix(), req.GetPath())
	requestType := "unsupported"
	if req.GetType() == gnmipb.GetRequest_ALL {
		requestType = "all"
	}
	encoding := "unknown"
	if name, ok := gnmipb.Encoding_name[int32(req.GetEncoding())]; ok {
		encoding = name
	}
	record := &gnmiGetAuditRecord{
		gnmiAuditCommon: newAuditCommon(ctx, requestID, auditMethodGet, started, len(req.GetPath()), paths, truncated),
		Get: gnmiGetFields{
			RequestType:         requestType,
			Encoding:            encoding,
			ModelCount:          len(req.GetUseModels()),
			ClientType:          clientNone,
			AuthorizedPathCount: 0,
		},
	}
	record.PathAuthzResult = getPathNotEvaluated
	return record
}

func (l *gnmiAuditLogger) newSetRecord(ctx context.Context, requestID string, req *gnmipb.SetRequest, started time.Time) *gnmiSetAuditRecord {
	allPaths := make([]*gnmipb.Path, 0, len(req.GetDelete())+len(req.GetReplace())+len(req.GetUpdate()))
	allPaths = append(allPaths, req.GetDelete()...)
	for _, update := range req.GetReplace() {
		allPaths = append(allPaths, update.GetPath())
	}
	for _, update := range req.GetUpdate() {
		allPaths = append(allPaths, update.GetPath())
	}
	paths, truncated := auditPaths(req.GetPrefix(), allPaths)
	record := &gnmiSetAuditRecord{
		gnmiAuditCommon: newAuditCommon(ctx, requestID, auditMethodSet, started, len(allPaths), paths, truncated),
		Set: gnmiSetFields{
			DeleteCount:     len(req.GetDelete()),
			ReplaceCount:    len(req.GetReplace()),
			UpdateCount:     len(req.GetUpdate()),
			Backend:         backendNone,
			ExecutionResult: executionNotStarted,
		},
	}
	record.PathAuthzResult = setPathNotEvaluated
	return record
}

func newAuditCommon(ctx context.Context, requestID string, method auditMethod, started time.Time, pathCount int, paths []string, truncated int) gnmiAuditCommon {
	peerType, peerAddress := sharedlog.PeerTypeAddr(ctx)
	return gnmiAuditCommon{
		Version:        gnmiAuditVersion,
		Method:         method,
		Time:           started.UTC().Format(time.RFC3339Nano),
		RequestID:      requestID,
		PeerType:       peerType,
		Peer:           peerAddress,
		IdentityState:  identityNotEvaluated,
		AccessResult:   accessNotEvaluated,
		Reason:         reasonCompleted,
		PathCount:      pathCount,
		Paths:          paths,
		PathsTruncated: truncated,
		started:        started,
	}
}

func (l *gnmiAuditLogger) emitGet(record *gnmiGetAuditRecord, err error) {
	record.finalize(l.now(), err)
	l.emit(record)
}

func (l *gnmiAuditLogger) emitSet(record *gnmiSetAuditRecord, err error) {
	record.finalize(l.now(), err)
	l.emit(record)
}

func (l *gnmiAuditLogger) finishGet(record *gnmiGetAuditRecord, rpcErr *error) {
	if panicValue := recover(); panicValue != nil {
		record.Reason = reasonHandlerPanic
		l.emitGet(record, status.Error(codes.Unknown, "handler panic"))
		panic(panicValue)
	}
	l.emitGet(record, errorValue(rpcErr))
}

func (l *gnmiAuditLogger) finishSet(record *gnmiSetAuditRecord, rpcErr *error) {
	if panicValue := recover(); panicValue != nil {
		record.Reason = reasonHandlerPanic
		record.Set.ExecutionResult = executionPanicked
		l.emitSet(record, status.Error(codes.Unknown, "handler panic"))
		panic(panicValue)
	}
	l.emitSet(record, errorValue(rpcErr))
}

func errorValue(err *error) error {
	if err == nil {
		return nil
	}
	return *err
}

func (c *gnmiAuditCommon) finalize(finished time.Time, err error) {
	c.GRPCCode = sharedlog.GRPCCode(err).String()
	c.DurationMS = finished.Sub(c.started).Milliseconds()
}

func (l *gnmiAuditLogger) emit(record any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.writer == nil {
		if l.connect == nil {
			l.recordLoss("connect")
			return
		}
		writer, err := l.connect()
		if err != nil {
			l.recordLoss("connect")
			return
		}
		l.writer = writer
	}

	err := sharedlog.WriteJSON(gnmiAuditPrefix, record, func(line string) error {
		return l.writer.Info(line)
	})
	if err == nil {
		return
	}

	if l.writer != nil {
		_ = l.writer.Close()
		l.writer = nil
	}
	switch {
	case errors.Is(err, sharedlog.ErrMarshal):
		l.recordLoss("marshal")
	case errors.Is(err, sharedlog.ErrSinkPanic):
		l.recordLoss("sink_panic")
	default:
		l.recordLoss("write")
	}
}

func (l *gnmiAuditLogger) recordLoss(class string) {
	l.lost()
	log.V(2).Infof("GNMI audit record lost: class=%s", class)
}

func auditPaths(prefix *gnmipb.Path, paths []*gnmipb.Path) ([]string, int) {
	result := make([]string, 0, min(len(paths), auditPathLimit))
	seen := make(map[string]struct{}, len(paths))
	truncated := 0
	for _, path := range paths {
		redacted := redactAuditPath(prefix, path)
		if _, ok := seen[redacted]; ok {
			continue
		}
		seen[redacted] = struct{}{}
		if len(result) == auditPathLimit {
			truncated++
			continue
		}
		result = append(result, redacted)
	}
	return result, truncated
}

func redactAuditPath(prefix, path *gnmipb.Path) string {
	var elems []*gnmipb.PathElem
	if prefix != nil {
		elems = append(elems, prefix.GetElem()...)
	}
	if path != nil {
		elems = append(elems, path.GetElem()...)
	}
	if len(elems) == 0 {
		return "/"
	}
	var builder strings.Builder
	for _, elem := range elems {
		builder.WriteByte('/')
		builder.WriteString(elem.GetName())
	}
	return builder.String()
}
