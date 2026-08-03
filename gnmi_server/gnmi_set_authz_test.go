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

type setPathAuthzResponse struct {
	result *pathz_authorizer.Result
	err    error
}

func (f *fakeSetPathAuthorizer) Authorize(
	user string,
	path *gnmipb.Path,
	mode pathzpb.Mode,
) (*pathz_authorizer.Result, error) {
	return f.AuthorizeWithPrefix(user, nil, path, mode)
}

func (f *fakeSetPathAuthorizer) UpdatePolicyFromFile(string) error { return nil }

func (f *fakeSetPathAuthorizer) UpdatePolicyFromProto(*pathzpb.AuthorizationPolicy) error {
	return nil
}

func (f *fakeSetPathAuthorizer) GetPolicy() *pathzpb.AuthorizationPolicy { return nil }

type fakeSetPathAuthorizer struct {
	responses []setPathAuthzResponse
	users     []string
	paths     []*gnmipb.Path
}

func (f *fakeSetPathAuthorizer) AuthorizeWithPrefix(
	user string,
	_ *gnmipb.Path,
	path *gnmipb.Path,
	_ pathzpb.Mode,
) (*pathz_authorizer.Result, error) {
	f.users = append(f.users, user)
	f.paths = append(f.paths, path)
	response := f.responses[len(f.paths)-1]
	return response.result, response.err
}

func TestAuthorizeSetRequest(t *testing.T) {
	path := func(name string) *gnmipb.Path {
		return &gnmipb.Path{Elem: []*gnmipb.PathElem{{Name: name}}}
	}

	var _ pathz_authorizer.GnmiAuthzProcessorInterface = (*fakeSetPathAuthorizer)(nil)
	request := &gnmipb.SetRequest{
		Delete:  []*gnmipb.Path{path("delete")},
		Replace: []*gnmipb.Update{{Path: path("replace")}},
		Update:  []*gnmipb.Update{{Path: path("update")}},
	}

	t.Run("all paths permitted", func(t *testing.T) {
		permit := &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_PERMIT}
		authz := &fakeSetPathAuthorizer{responses: []setPathAuthzResponse{
			{result: permit}, {result: permit}, {result: permit},
		}}
		result, err := authorizeSetRequest(authz, "validated-user", request)
		if err != nil || result != setPathAllowed {
			t.Fatalf("authorizeSetRequest() = %q, %v", result, err)
		}
		if len(authz.users) != 3 {
			t.Fatalf("authorization calls = %d, want 3", len(authz.users))
		}
		for _, user := range authz.users {
			if user != "validated-user" {
				t.Fatalf("authorization user = %q", user)
			}
		}
	})

	t.Run("deny stops at first denied path", func(t *testing.T) {
		authz := &fakeSetPathAuthorizer{responses: []setPathAuthzResponse{{
			result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_DENY},
		}}}
		result, err := authorizeSetRequest(authz, "admin", request)
		if status.Code(err) != codes.PermissionDenied || result != setPathDenied {
			t.Fatalf("authorizeSetRequest() = %q, %v", result, err)
		}
		if len(authz.paths) != 1 {
			t.Fatalf("authorization calls = %d, want 1", len(authz.paths))
		}
	})

	t.Run("unspecified is denied", func(t *testing.T) {
		authz := &fakeSetPathAuthorizer{responses: []setPathAuthzResponse{{
			result: &pathz_authorizer.Result{Action: pathzpb.Action_ACTION_UNSPECIFIED},
		}}}
		result, err := authorizeSetRequest(authz, "admin", request)
		if status.Code(err) != codes.PermissionDenied || result != setPathDenied {
			t.Fatalf("authorizeSetRequest() = %q, %v", result, err)
		}
	})

	t.Run("evaluator error is internal and redacted", func(t *testing.T) {
		authz := &fakeSetPathAuthorizer{responses: []setPathAuthzResponse{{
			err: errors.New("SENSITIVE_POLICY_ERROR"),
		}}}
		result, err := authorizeSetRequest(authz, "admin", request)
		if status.Code(err) != codes.Internal || result != setPathError {
			t.Fatalf("authorizeSetRequest() = %q, %v", result, err)
		}
		if status.Convert(err).Message() == "SENSITIVE_POLICY_ERROR" {
			t.Fatal("pathz evaluator error leaked to client")
		}
	})
}
