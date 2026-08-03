package gnmi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	gnmi_extpb "github.com/openconfig/gnmi/proto/gnmi_ext"
	pathzpb "github.com/openconfig/gnsi/pathz"
	"github.com/sonic-net/sonic-gnmi/common_utils"
	"github.com/sonic-net/sonic-gnmi/pathz_authorizer"
	spb "github.com/sonic-net/sonic-gnmi/proto"
	sdc "github.com/sonic-net/sonic-gnmi/sonic_data_client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetAuditTranslibOutcomesAndFiltering(t *testing.T) {
	tests := []struct {
		name                string
		authErr             error
		getErr              error
		pathzResponses      []setPathAuthzResponse
		constructorErr      error
		wantCode            codes.Code
		wantReason          string
		wantConstructorCall int
		wantGetCalls        int
		wantPaths           int
		wantPathz           string
		unsupportedType     bool
		invalidEncoding     bool
	}{
		{
			name:            "unsupported type before authentication",
			wantCode:        codes.Unimplemented,
			wantReason:      "unsupported_type",
			wantPathz:       "not_evaluated",
			unsupportedType: true,
		},
		{
			name:            "invalid encoding before authentication",
			wantCode:        codes.Unimplemented,
			wantReason:      "encoding_model_error",
			wantPathz:       "not_evaluated",
			invalidEncoding: true,
		},
		{
			name:                "success omits response values",
			wantCode:            codes.OK,
			wantReason:          "completed",
			wantConstructorCall: 1,
			wantGetCalls:        1,
			wantPaths:           2,
			wantPathz:           "not_enabled",
		},
		{
			name:                "backend failure",
			getErr:              errors.New("backend failed"),
			wantCode:            codes.NotFound,
			wantReason:          "backend_failed",
			wantConstructorCall: 1,
			wantGetCalls:        1,
			wantPaths:           2,
			wantPathz:           "not_enabled",
		},
		{
			name:                "authentication denial before constructor",
			authErr:             status.Error(codes.Unauthenticated, "denied"),
			wantCode:            codes.Unauthenticated,
			wantReason:          "access_denied",
			wantConstructorCall: 0,
			wantGetCalls:        0,
			wantPathz:           "not_evaluated",
		},
		{
			name: "partial pathz passes only permitted path",
			pathzResponses: []setPathAuthzResponse{
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT}},
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY}},
			},
			wantCode:            codes.OK,
			wantReason:          "completed",
			wantConstructorCall: 1,
			wantGetCalls:        1,
			wantPaths:           1,
			wantPathz:           "partial_allowed",
		},
		{
			name: "all pathz denied before constructor",
			pathzResponses: []setPathAuthzResponse{
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY}},
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_UNSPECIFIED}},
			},
			wantCode:            codes.PermissionDenied,
			wantReason:          "path_policy_denied",
			wantConstructorCall: 0,
			wantGetCalls:        0,
			wantPathz:           "all_denied",
		},
		{
			name: "pathz evaluator error before constructor",
			pathzResponses: []setPathAuthzResponse{
				{err: errors.New("SENSITIVE_PATHZ_ERROR")},
			},
			wantCode:            codes.Internal,
			wantReason:          "path_policy_error",
			wantConstructorCall: 0,
			wantGetCalls:        0,
			wantPathz:           "error",
		},
		{
			name:                "constructor failure after access",
			constructorErr:      errors.New("constructor failed"),
			wantCode:            codes.NotFound,
			wantReason:          "client_create_failed",
			wantConstructorCall: 1,
			wantGetCalls:        0,
			wantPaths:           2,
			wantPathz:           "not_enabled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := &fakeAuditSetClient{
				getErr: tc.getErr,
				getValues: []*spb.Value{{
					Path: &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: "result"}}},
					Val: &gnmipb.TypedValue{
						Value: &gnmipb.TypedValue_StringVal{StringVal: "SECRET_RESPONSE_VALUE"},
					},
				}},
			}
			constructorCalls := 0
			authCalls := 0
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(authenticate,
				func(_ *Config, ctx context.Context, _ string, _ bool) (context.Context, error) {
					authCalls++
					rc, ctx := common_utils.GetContext(ctx)
					rc.Auth.AuthEnabled = true
					if tc.authErr == nil {
						rc.Auth.User = "validated-user"
					}
					return ctx, tc.authErr
				})
			patches.ApplyFunc(sdc.NewTranslClient,
				func(_ *gnmipb.Path, paths []*gnmipb.Path, _ context.Context, _ []*gnmi_extpb.Extension, _ ...sdc.TranslClientOption) (sdc.Client, error) {
					constructorCalls++
					if len(paths) != tc.wantPaths {
						t.Fatalf("constructor paths = %d, want %d", len(paths), tc.wantPaths)
					}
					if tc.constructorErr != nil {
						return nil, tc.constructorErr
					}
					return fakeClient, nil
				})

			writer := &auditTestWriter{}
			originalLogger := defaultGNMIAuditLogger
			defaultGNMIAuditLogger = newGNMIAuditLogger(
				func() (auditSyslogWriter, error) { return writer, nil }, nil, func() {},
			)
			defer func() { defaultGNMIAuditLogger = originalLogger }()
			server := &Server{
				config: &Config{PathzPolicy: len(tc.pathzResponses) > 0},
			}
			var authorizer *fakeSetPathAuthorizer
			if len(tc.pathzResponses) > 0 {
				authorizer = &fakeSetPathAuthorizer{responses: tc.pathzResponses}
				server.gnsiPathz = &GNSIPathzServer{
					pathzProcessor: authorizer,
				}
			}
			request := &gnmipb.GetRequest{
				Type:     gnmipb.GetRequest_ALL,
				Encoding: gnmipb.Encoding_JSON_IETF,
				Path: []*gnmipb.Path{
					{Origin: "openconfig", Elem: []*gnmipb.PathElem{{Name: "interfaces"}}},
					{Origin: "openconfig", Elem: []*gnmipb.PathElem{{Name: "system"}}},
				},
			}
			if tc.unsupportedType {
				request.Type = gnmipb.GetRequest_STATE
			}
			if tc.invalidEncoding {
				request.Encoding = gnmipb.Encoding_ASCII
			}

			_, err := server.Get(context.Background(), request)
			if status.Code(err) != tc.wantCode ||
				constructorCalls != tc.wantConstructorCall ||
				fakeClient.getCalls != tc.wantGetCalls {
				t.Fatalf("Get()=%v constructor/get calls=%d/%d", err, constructorCalls, fakeClient.getCalls)
			}
			wantAuthCalls := 1
			if tc.unsupportedType || tc.invalidEncoding {
				wantAuthCalls = 0
			}
			if authCalls != wantAuthCalls {
				t.Fatalf("authenticate calls = %d, want %d", authCalls, wantAuthCalls)
			}
			if authorizer != nil {
				for _, user := range authorizer.users {
					if user != "validated-user" {
						t.Fatalf("pathz user = %q, want validated-user", user)
					}
				}
			}
			lines, calls, _ := writer.snapshot()
			if calls != 1 || len(lines) != 1 {
				t.Fatalf("audit calls/lines = %d/%d", calls, len(lines))
			}
			if strings.Contains(lines[0], "SECRET_RESPONSE_VALUE") {
				t.Fatalf("audit leaked response value: %s", lines[0])
			}
			fields := decodeAuditLine(t, lines[0])
			getFields := fields["get"].(map[string]any)
			if fields["reason"] != tc.wantReason ||
				fields["grpc_code"] != tc.wantCode.String() ||
				fields["path_authz_result"] != tc.wantPathz {
				t.Fatalf("audit fields = %#v", fields)
			}
			if tc.wantPathz == "partial_allowed" && getFields["authorized_path_count"] != float64(1) {
				t.Fatalf("authorized_path_count = %v", getFields["authorized_path_count"])
			}
		})
	}
}

func TestGetAuditOperationalUsesGNOIAuthBeforeHandler(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	authCalls := 0
	patches.ApplyFunc(authenticate,
		func(_ *Config, ctx context.Context, target string, write bool) (context.Context, error) {
			authCalls++
			if target != "gnoi" || write {
				t.Fatalf("authenticate target/write = %q/%v", target, write)
			}
			return ctx, status.Error(codes.Unauthenticated, "denied")
		})

	writer := &auditTestWriter{}
	originalLogger := defaultGNMIAuditLogger
	defaultGNMIAuditLogger = newGNMIAuditLogger(
		func() (auditSyslogWriter, error) { return writer, nil }, nil, func() {},
	)
	defer func() { defaultGNMIAuditLogger = originalLogger }()

	server := &Server{config: &Config{}}
	_, err := server.Get(context.Background(), &gnmipb.GetRequest{
		Prefix:   &gnmipb.Path{Target: "OPERATIONAL"},
		Encoding: gnmipb.Encoding_JSON_IETF,
		Path:     []*gnmipb.Path{{Elem: []*gnmipb.PathElem{{Name: "disk"}}}},
	})
	if status.Code(err) != codes.Unauthenticated || authCalls != 1 {
		t.Fatalf("Get() = %v, auth calls=%d", err, authCalls)
	}
	lines, calls, _ := writer.snapshot()
	if calls != 1 || len(lines) != 1 {
		t.Fatalf("audit calls/lines = %d/%d, want 1/1", calls, len(lines))
	}
	fields := decodeAuditLine(t, lines[0])
	getFields := fields["get"].(map[string]any)
	if getFields["client_type"] != "operational" || fields["reason"] != "access_denied" {
		t.Fatalf("audit fields = %#v", fields)
	}
}
