package gnmi

import (
	"errors"
	"testing"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	pathzpb "github.com/openconfig/gnsi/pathz"
	"github.com/sonic-net/sonic-gnmi/pathz_authorizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthorizeGetRequest(t *testing.T) {
	path := func(name string) *gnmipb.Path {
		return &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: name}}}
	}
	request := &gnmipb.GetRequest{Path: []*gnmipb.Path{path("one"), path("two")}}

	tests := []struct {
		name       string
		responses  []setPathAuthzResponse
		wantResult pathAuthzResult
		wantPaths  int
		wantCode   codes.Code
	}{
		{
			name: "all allowed",
			responses: []setPathAuthzResponse{
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT}},
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT}},
			},
			wantResult: getPathAllAllowed,
			wantPaths:  2,
			wantCode:   codes.OK,
		},
		{
			name: "partial allowed",
			responses: []setPathAuthzResponse{
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT}},
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY}},
			},
			wantResult: getPathPartial,
			wantPaths:  1,
			wantCode:   codes.OK,
		},
		{
			name: "all denied including unspecified",
			responses: []setPathAuthzResponse{
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY}},
				{result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_UNSPECIFIED}},
			},
			wantResult: getPathAllDenied,
			wantCode:   codes.PermissionDenied,
		},
		{
			name: "evaluator error",
			responses: []setPathAuthzResponse{
				{err: errors.New("SENSITIVE_PATHZ_ERROR")},
			},
			wantResult: getPathError,
			wantCode:   codes.Internal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authorizer := &fakeSetPathAuthorizer{responses: tc.responses}
			paths, result, err := authorizeGetRequest(authorizer, "validated-user", request)
			if status.Code(err) != tc.wantCode || result != tc.wantResult || len(paths) != tc.wantPaths {
				t.Fatalf("authorizeGetRequest() = %d, %q, %v", len(paths), result, err)
			}
			for _, user := range authorizer.users {
				if user != "validated-user" {
					t.Fatalf("authorization user = %q", user)
				}
			}
			if err != nil && status.Convert(err).Message() == "SENSITIVE_PATHZ_ERROR" {
				t.Fatal("pathz error leaked to client")
			}
		})
	}
}

func TestResolveGetRoute(t *testing.T) {
	tests := []struct {
		name       string
		request    *gnmipb.GetRequest
		wantTarget string
		wantClient clientType
		wantOrigin string
		wantCode   codes.Code
	}{
		{
			name:       "operational preserves gnoi roles",
			request:    &gnmipb.GetRequest{Prefix: &gnmipb.Path{Target: "OPERATIONAL"}},
			wantTarget: "gnoi",
			wantClient: clientOperational,
		},
		{
			name:       "direct db",
			request:    &gnmipb.GetRequest{Prefix: &gnmipb.Path{Target: "CONFIG_DB"}},
			wantTarget: "gnmi_CONFIG_DB",
			wantClient: clientDB,
		},
		{
			name: "native db from path without probing",
			request: &gnmipb.GetRequest{
				Prefix: &gnmipb.Path{Origin: "sonic-db"},
				Path: []*gnmipb.Path{{Elem: []*gnmipb.PathElem{
					{Name: "CONFIG_DB"}, {Name: "localhost"}, {Name: "PORT"},
				}}},
			},
			wantTarget: "gnmi_CONFIG_DB",
			wantClient: clientNative,
			wantOrigin: "sonic-db",
		},
		{
			name: "translib",
			request: &gnmipb.GetRequest{
				Path: []*gnmipb.Path{{Origin: "openconfig", Elem: []*gnmipb.PathElem{{Name: "interfaces"}}}},
			},
			wantTarget: "gnmi",
			wantClient: clientTranslib,
			wantOrigin: "openconfig",
		},
		{
			name: "conflicting origins",
			request: &gnmipb.GetRequest{Path: []*gnmipb.Path{
				{Origin: "openconfig"}, {Origin: "sonic-db"},
			}},
			wantCode: codes.Unimplemented,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route, err := resolveGetRoute(tc.request)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("resolveGetRoute() error = %v", err)
			}
			if err == nil && (route.authTarget != tc.wantTarget ||
				route.clientType != tc.wantClient ||
				route.origin != tc.wantOrigin) {
				t.Fatalf("route = %#v", route)
			}
		})
	}
}
