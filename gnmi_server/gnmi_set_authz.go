package gnmi

import (
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	pathzpb "github.com/openconfig/gnsi/pathz"
	"github.com/sonic-net/sonic-gnmi/pathz_authorizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type setPathAuthorizer interface {
	AuthorizeWithPrefix(
		user string,
		prefix *gnmipb.Path,
		path *gnmipb.Path,
		mode pathzpb.Mode,
	) (*pathz_authorizer.Result, error)
}

func authorizeSetRequest(authorizer setPathAuthorizer, user string, req *gnmipb.SetRequest) (pathAuthzResult, error) {
	paths := make([]*gnmipb.Path, 0, len(req.GetDelete())+len(req.GetReplace())+len(req.GetUpdate()))
	paths = append(paths, req.GetDelete()...)
	for _, update := range req.GetReplace() {
		paths = append(paths, update.GetPath())
	}
	for _, update := range req.GetUpdate() {
		paths = append(paths, update.GetPath())
	}

	for _, path := range paths {
		result, err := authorizer.AuthorizeWithPrefix(user, req.GetPrefix(), path, pathzpb.Mode_MODE_WRITE)
		if err != nil {
			return setPathError, status.Error(codes.Internal, "Path authorization failed.")
		}
		if result == nil || result.Action != pathzpb.Action_ACTION_PERMIT {
			return setPathDenied, status.Error(codes.PermissionDenied, "Unauthorized request. Rejected by pathz policy.")
		}
	}
	return setPathAllowed, nil
}
