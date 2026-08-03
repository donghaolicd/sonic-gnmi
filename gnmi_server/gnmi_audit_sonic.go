package gnmi

import "github.com/sonic-net/sonic-gnmi/common_utils"

var defaultGNMIAuditLogger = newProductionGNMIAuditLogger(
	func() { common_utils.IncCounter(common_utils.GNMI_AUDIT_LOST) },
)
