package gnmi

import (
	"strings"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	pathzpb "github.com/openconfig/gnsi/pathz"
	spb "github.com/sonic-net/sonic-gnmi/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type getRoute struct {
	authTarget string
	clientType clientType
	origin     string
	target     string
	dbName     string
}

func authorizeGetRequest(authorizer setPathAuthorizer, user string, req *gnmipb.GetRequest) ([]*gnmipb.Path, pathAuthzResult, error) {
	authorized := make([]*gnmipb.Path, 0, len(req.GetPath()))
	for _, path := range req.GetPath() {
		result, err := authorizer.AuthorizeWithPrefix(user, req.GetPrefix(), path, pathzpb.Mode_MODE_READ)
		if err != nil {
			return nil, getPathError, status.Error(codes.Internal, "Path authorization failed.")
		}
		if result != nil && result.Action == pathzpb.Action_ACTION_PERMIT {
			authorized = append(authorized, path)
		}
	}
	if len(authorized) == 0 && len(req.GetPath()) > 0 {
		return nil, getPathAllDenied, status.Error(codes.PermissionDenied, "Unauthorized request. Rejected by pathz policy.")
	}
	if len(authorized) == len(req.GetPath()) {
		return authorized, getPathAllAllowed, nil
	}
	return authorized, getPathPartial, nil
}

func resolveGetRoute(req *gnmipb.GetRequest) (getRoute, error) {
	route := getRoute{
		authTarget: "gnmi",
		clientType: clientTranslib,
	}
	if prefix := req.GetPrefix(); prefix != nil {
		route.target = prefix.GetTarget()
		route.origin = prefix.GetOrigin()
	}

	switch route.target {
	case "OPERATIONAL":
		route.authTarget = "gnoi"
		route.clientType = clientOperational
		return route, nil
	case "OTHERS":
		route.authTarget = "gnmi_other"
		route.clientType = clientNonDB
		return route, nil
	case "SHOW":
		route.authTarget = "gnmi_show"
		route.clientType = clientShow
		return route, nil
	}

	if dbName, ok := targetDatabaseName(route.target); ok {
		route.dbName = dbName
		route.authTarget = "gnmi_" + dbName
		route.clientType = clientDB
		return route, nil
	}

	if route.origin == "" {
		origin, err := getRequestOrigin(req.GetPath())
		if err != nil {
			return getRoute{}, err
		}
		route.origin = origin
	}
	if route.origin != "sonic-db" {
		return route, nil
	}

	dbName, err := nativeDatabaseName(req.GetPrefix(), req.GetPath())
	if err != nil {
		return getRoute{}, err
	}
	route.dbName = dbName
	route.authTarget = "gnmi_" + dbName
	route.clientType = clientNative
	return route, nil
}

func targetDatabaseName(target string) (string, bool) {
	dbName := strings.Split(target, "/")[0]
	_, ok := spb.Target_value[dbName]
	return dbName, ok
}

func getRequestOrigin(paths []*gnmipb.Path) (string, error) {
	origin := ""
	for i, path := range paths {
		if i == 0 {
			origin = path.GetOrigin()
		} else if origin != path.GetOrigin() {
			return "", status.Error(codes.Unimplemented, "Origin conflict in path")
		}
	}
	return origin, nil
}

func nativeDatabaseName(prefix *gnmipb.Path, paths []*gnmipb.Path) (string, error) {
	dbName := ""
	for _, path := range paths {
		elems := make([]*gnmipb.PathElem, 0, len(prefix.GetElem())+len(path.GetElem()))
		elems = append(elems, prefix.GetElem()...)
		elems = append(elems, path.GetElem()...)
		if len(elems) == 0 || elems[0].GetName() == "" {
			return "", status.Error(codes.Unimplemented, "No valid database target")
		}
		if dbName == "" {
			dbName = elems[0].GetName()
		} else if dbName != elems[0].GetName() {
			return "", status.Error(codes.Unimplemented, "Target conflict in path")
		}
	}
	if dbName == "" {
		return "", status.Error(codes.Unimplemented, "No valid path")
	}
	return dbName, nil
}
