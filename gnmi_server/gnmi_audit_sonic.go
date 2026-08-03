package gnmi

import (
	"context"

	"github.com/sonic-net/sonic-gnmi/common_utils"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var defaultGNMIAuditLogger = newProductionGNMIAuditLogger(
	func() { common_utils.IncCounter(common_utils.GNMI_AUDIT_LOST) },
)

func transportValidatedPrincipal(ctx context.Context) string {
	requestPeer, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	tlsInfo, ok := requestPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return ""
	}
	return tlsInfo.State.VerifiedChains[0][0].Subject.CommonName
}
