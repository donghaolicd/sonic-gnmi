package gnmi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Workiva/go-datastructures/queue"
	"github.com/agiledragon/gomonkey/v2"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	gnmi_extpb "github.com/openconfig/gnmi/proto/gnmi_ext"
	pathzpb "github.com/openconfig/gnsi/pathz"
	"github.com/sonic-net/sonic-gnmi/common_utils"
	"github.com/sonic-net/sonic-gnmi/pathz_authorizer"
	"github.com/sonic-net/sonic-gnmi/pkg/bypass"
	spb "github.com/sonic-net/sonic-gnmi/proto"
	sdc "github.com/sonic-net/sonic-gnmi/sonic_data_client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeAuditSetClient struct {
	sdc.Client
	setErr    error
	setCalls  int
	getErr    error
	getCalls  int
	getValues []*spb.Value
}

func (c *fakeAuditSetClient) Set([]*gnmipb.Path, []*gnmipb.Update, []*gnmipb.Update) error {
	c.setCalls++
	return c.setErr
}

func (c *fakeAuditSetClient) Close() error { return nil }

func (c *fakeAuditSetClient) StreamRun(*queue.PriorityQueue, chan struct{}, *sync.WaitGroup, *gnmipb.SubscriptionList) {
}
func (c *fakeAuditSetClient) PollRun(*queue.PriorityQueue, chan struct{}, *sync.WaitGroup, *gnmipb.SubscriptionList) {
}
func (c *fakeAuditSetClient) OnceRun(*queue.PriorityQueue, chan struct{}, *sync.WaitGroup, *gnmipb.SubscriptionList) {
}
func (c *fakeAuditSetClient) Get(*sync.WaitGroup) ([]*spb.Value, error) {
	c.getCalls++
	return c.getValues, c.getErr
}
func (c *fakeAuditSetClient) Capabilities() []gnmipb.ModelData { return nil }
func (c *fakeAuditSetClient) FailedSend()                      {}
func (c *fakeAuditSetClient) SentOne(*sdc.Value)               {}

func TestSetAuditEarlyReturns(t *testing.T) {
	tests := []struct {
		name       string
		server     *Server
		request    *gnmipb.SetRequest
		wantCode   codes.Code
		wantReason string
	}{
		{
			name: "not master",
			server: &Server{
				config: &Config{},
				ReqFromMaster: func(*gnmipb.SetRequest, *uint128) error {
					return status.Error(codes.Aborted, "not master")
				},
			},
			request:    &gnmipb.SetRequest{},
			wantCode:   codes.Aborted,
			wantReason: "not_master",
		},
		{
			name: "read only",
			server: &Server{
				config:        &Config{},
				ReqFromMaster: ReqFromMasterDisabledMA,
			},
			request:    &gnmipb.SetRequest{},
			wantCode:   codes.Unimplemented,
			wantReason: "read_only",
		},
		{
			name: "invalid origin",
			server: &Server{
				config: &Config{
					EnableNativeWrite:   true,
					EnableTranslibWrite: true,
				},
				ReqFromMaster: ReqFromMasterDisabledMA,
			},
			request: &gnmipb.SetRequest{
				Delete: []*gnmipb.Path{
					{Origin: "sonic-db", Elem: []*gnmipb.PathElem{{Name: "CONFIG_DB"}}},
					{Origin: "openconfig", Elem: []*gnmipb.PathElem{{Name: "interfaces"}}},
				},
			},
			wantCode:   codes.Unimplemented,
			wantReason: "invalid_origin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writer := &auditTestWriter{}
			logger := newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil },
				nil,
				func() {},
			)
			originalLogger := defaultGNMIAuditLogger
			defaultGNMIAuditLogger = logger
			defer func() { defaultGNMIAuditLogger = originalLogger }()

			_, err := tc.server.Set(context.Background(), tc.request)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("Set() code = %v, want %v (error %v)", status.Code(err), tc.wantCode, err)
			}
			lines, calls, _ := writer.snapshot()
			if calls != 1 || len(lines) != 1 {
				t.Fatalf("audit calls/lines = %d/%d, want 1/1", calls, len(lines))
			}
			const prefix = "GNMI_AUDIT "
			var fields map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], prefix)), &fields); err != nil {
				t.Fatalf("invalid audit JSON: %v", err)
			}
			if fields["method"] != "set" ||
				fields["reason"] != tc.wantReason ||
				fields["grpc_code"] != tc.wantCode.String() {
				t.Fatalf("audit fields = %#v", fields)
			}
		})
	}
}

func TestSetAuditLoggerDoesNotReplaceRPCError(t *testing.T) {
	writer := &auditTestWriter{infoErr: errors.New("audit unavailable")}
	logger := newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil },
		nil,
		func() {},
	)
	originalLogger := defaultGNMIAuditLogger
	defaultGNMIAuditLogger = logger
	defer func() { defaultGNMIAuditLogger = originalLogger }()

	server := &Server{
		config: &Config{},
		ReqFromMaster: func(*gnmipb.SetRequest, *uint128) error {
			return status.Error(codes.Aborted, "not master")
		},
	}
	_, err := server.Set(context.Background(), &gnmipb.SetRequest{})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("Set() code = %v, want Aborted", status.Code(err))
	}
}

func TestSetAuditTranslibOutcomeAndAccessOrdering(t *testing.T) {
	tests := []struct {
		name          string
		authErr       error
		setErr        error
		pathzResponse *pathz_authorizer.Result
		wantCode      codes.Code
		wantReason    string
		wantExecution string
		wantSetCalls  int
	}{
		{
			name:          "success",
			pathzResponse: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT},
			wantCode:      codes.OK,
			wantReason:    "completed",
			wantExecution: "succeeded",
			wantSetCalls:  1,
		},
		{
			name:          "backend failure",
			setErr:        errors.New("backend failed"),
			pathzResponse: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT},
			wantCode:      codes.Unknown,
			wantReason:    "backend_failed",
			wantExecution: "failed",
			wantSetCalls:  1,
		},
		{
			name:          "authentication denial has no Set effect",
			authErr:       status.Error(codes.Unauthenticated, "denied"),
			wantCode:      codes.Unauthenticated,
			wantReason:    "access_denied",
			wantExecution: "not_started",
			wantSetCalls:  0,
		},
		{
			name:          "pathz denial has no Set effect",
			pathzResponse: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY},
			wantCode:      codes.PermissionDenied,
			wantReason:    "path_policy_denied",
			wantExecution: "not_started",
			wantSetCalls:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := &fakeAuditSetClient{setErr: tc.setErr}
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(sdc.NewTranslClient,
				func(*gnmipb.Path, []*gnmipb.Path, context.Context, []*gnmi_extpb.Extension, ...sdc.TranslClientOption) (sdc.Client, error) {
					return fakeClient, nil
				})
			patches.ApplyFunc(authenticate,
				func(_ *Config, ctx context.Context, _ string, _ bool) (context.Context, error) {
					rc, ctx := common_utils.GetContext(ctx)
					rc.Auth.AuthEnabled = true
					if tc.authErr == nil {
						rc.Auth.User = "validated-user"
					}
					return ctx, tc.authErr
				})

			writer := &auditTestWriter{}
			logger := newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil },
				nil,
				func() {},
			)
			originalLogger := defaultGNMIAuditLogger
			defaultGNMIAuditLogger = logger
			defer func() { defaultGNMIAuditLogger = originalLogger }()

			server := &Server{
				config: &Config{
					EnableTranslibWrite: true,
					PathzPolicy:         tc.pathzResponse != nil,
				},
				ReqFromMaster:     ReqFromMasterDisabledMA,
				SaveStartupConfig: saveOnSetDisabled,
			}
			if tc.pathzResponse != nil {
				server.gnsiPathz = &GNSIPathzServer{pathzProcessor: &fakeSetPathAuthorizer{
					responses: []setPathAuthzResponse{{result: tc.pathzResponse}},
				}}
			}
			request := &gnmipb.SetRequest{Update: []*gnmipb.Update{{
				Path: &gnmipb.Path{
					Origin: "openconfig",
					Elem:   []*gnmipb.PathElem{{Name: "interfaces"}},
				},
				Val: &gnmipb.TypedValue{
					Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(`{"secret":"DO_NOT_LOG"}`)},
				},
			}}}

			_, err := server.Set(context.Background(), request)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("Set() code = %v, want %v (error %v)", status.Code(err), tc.wantCode, err)
			}
			if fakeClient.setCalls != tc.wantSetCalls {
				t.Fatalf("Set calls = %d, want %d", fakeClient.setCalls, tc.wantSetCalls)
			}
			lines, calls, _ := writer.snapshot()
			if calls != 1 || len(lines) != 1 {
				t.Fatalf("audit calls/lines = %d/%d, want 1/1", calls, len(lines))
			}
			if strings.Contains(lines[0], "DO_NOT_LOG") {
				t.Fatalf("audit leaked Set value: %s", lines[0])
			}
			fields := decodeAuditLine(t, lines[0])
			setFields := fields["set"].(map[string]any)
			if fields["reason"] != tc.wantReason ||
				fields["grpc_code"] != tc.wantCode.String() ||
				setFields["execution_result"] != tc.wantExecution {
				t.Fatalf("audit fields = %#v", fields)
			}
		})
	}
}

func TestBasicAuthFailureDoesNotPopulatePrincipal(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(UserPwAuth, func(string, string) (bool, error) {
		return false, nil
	})
	populateCalled := false
	patches.ApplyFunc(PopulateAuthStruct, func(string, *common_utils.AuthInfo, []string) error {
		populateCalled = true
		return nil
	})
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs("username", "caller-controlled", "password", "wrong"),
	)
	rc, ctx := common_utils.GetContext(ctx)
	_, err := BasicAuthenAndAuthor(ctx)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("BasicAuthenAndAuthor() code = %v", status.Code(err))
	}
	if populateCalled || rc.Auth.User != "" {
		t.Fatalf("failed basic auth populated principal: called=%v user=%q", populateCalled, rc.Auth.User)
	}
}

func TestSetAuditNativeOutcome(t *testing.T) {
	tests := []struct {
		name          string
		setErr        error
		wantCode      codes.Code
		wantReason    string
		wantExecution string
	}{
		{name: "success", wantCode: codes.OK, wantReason: "completed", wantExecution: "succeeded"},
		{name: "failure", setErr: errors.New("native failed"), wantCode: codes.Unknown, wantReason: "backend_failed", wantExecution: "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := &fakeAuditSetClient{setErr: tc.setErr}
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(sdc.NewMixedDbClient,
				func(_ []*gnmipb.Path, _ *gnmipb.Path, _ string, _ gnmipb.Encoding, _ string, _ string, target *string) (sdc.Client, error) {
					*target = "CONFIG_DB"
					return fakeClient, nil
				})
			patches.ApplyFunc(authenticate,
				func(_ *Config, ctx context.Context, target string, write bool) (context.Context, error) {
					if target != "gnmi_CONFIG_DB" || !write {
						t.Fatalf("authenticate target/write = %q/%v", target, write)
					}
					rc, ctx := common_utils.GetContext(ctx)
					rc.Auth.AuthEnabled = true
					rc.Auth.User = "validated-user"
					return ctx, nil
				})
			writer := &auditTestWriter{}
			originalLogger := defaultGNMIAuditLogger
			defaultGNMIAuditLogger = newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil }, nil, func() {},
			)
			defer func() { defaultGNMIAuditLogger = originalLogger }()

			server := &Server{
				config:            &Config{EnableNativeWrite: true},
				ReqFromMaster:     ReqFromMasterDisabledMA,
				SaveStartupConfig: saveOnSetDisabled,
			}
			request := &gnmipb.SetRequest{
				Prefix: &gnmipb.Path{Origin: "sonic-db"},
				Update: []*gnmipb.Update{{
					Path: &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: "CONFIG_DB"}, {Name: "localhost"}, {Name: "PORT"}}},
					Val:  &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(`{}`)}},
				}},
			}
			_, err := server.Set(context.Background(), request)
			if status.Code(err) != tc.wantCode || fakeClient.setCalls != 1 {
				t.Fatalf("Set() = %v, calls=%d", err, fakeClient.setCalls)
			}
			lines, _, _ := writer.snapshot()
			fields := decodeAuditLine(t, lines[0])
			setFields := fields["set"].(map[string]any)
			if fields["reason"] != tc.wantReason ||
				setFields["backend"] != "native" ||
				setFields["execution_result"] != tc.wantExecution {
				t.Fatalf("audit fields = %#v", fields)
			}
		})
	}
}

func TestSetAuditBypassOrdering(t *testing.T) {
	tests := []struct {
		name          string
		authErr       error
		bypassErr     error
		wantCode      codes.Code
		wantExecution string
		wantTryCalls  int
	}{
		{name: "success", wantCode: codes.OK, wantExecution: "succeeded", wantTryCalls: 1},
		{name: "failure", bypassErr: errors.New("bypass failed"), wantCode: codes.Internal, wantExecution: "failed", wantTryCalls: 1},
		{name: "access denied before bypass", authErr: status.Error(codes.Unauthenticated, "denied"), wantCode: codes.Unauthenticated, wantExecution: "not_started"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(bypass.ShouldBypassSet,
				func(context.Context, *gnmipb.Path, []*gnmipb.Path, []*gnmipb.Update) bool {
					return true
				})
			authDone := false
			patches.ApplyFunc(authenticate,
				func(_ *Config, ctx context.Context, target string, write bool) (context.Context, error) {
					if target != "gnmi_config_db" || !write {
						t.Fatalf("authenticate target/write = %q/%v", target, write)
					}
					authDone = true
					rc, ctx := common_utils.GetContext(ctx)
					rc.Auth.AuthEnabled = true
					if tc.authErr == nil {
						rc.Auth.User = "validated-user"
					}
					return ctx, tc.authErr
				})
			authz := &fakeSetPathAuthorizer{responses: []setPathAuthzResponse{{
				result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT},
			}}}
			tryCalls := 0
			patches.ApplyFunc(bypass.TrySet,
				func(context.Context, *gnmipb.Path, []*gnmipb.Path, []*gnmipb.Update) (*gnmipb.SetResponse, bool, error) {
					tryCalls++
					if !authDone || len(authz.users) != 1 {
						t.Fatal("bypass executed before authenticate/pathz")
					}
					return &gnmipb.SetResponse{}, true, tc.bypassErr
				})

			writer := &auditTestWriter{}
			originalLogger := defaultGNMIAuditLogger
			defaultGNMIAuditLogger = newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil }, nil, func() {},
			)
			defer func() { defaultGNMIAuditLogger = originalLogger }()
			server := &Server{
				config: &Config{
					EnableNativeWrite: true,
					PathzPolicy:       true,
				},
				ReqFromMaster: ReqFromMasterDisabledMA,
				gnsiPathz:     &GNSIPathzServer{pathzProcessor: authz},
			}
			request := &gnmipb.SetRequest{
				Prefix: &gnmipb.Path{Origin: "sonic-db"},
				Update: []*gnmipb.Update{{
					Path: &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: "CONFIG_DB"}, {Name: "localhost"}, {Name: "VNET"}}},
					Val:  &gnmipb.TypedValue{Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(`{}`)}},
				}},
			}
			_, err := server.Set(context.Background(), request)
			if status.Code(err) != tc.wantCode || tryCalls != tc.wantTryCalls {
				t.Fatalf("Set() = %v, try calls=%d, want code/calls=%v/%d", err, tryCalls, tc.wantCode, tc.wantTryCalls)
			}
			lines, _, _ := writer.snapshot()
			setFields := decodeAuditLine(t, lines[0])["set"].(map[string]any)
			if setFields["backend"] != "bypass" || setFields["execution_result"] != tc.wantExecution {
				t.Fatalf("set fields = %#v", setFields)
			}
		})
	}
}
