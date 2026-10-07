// Package all imports every built-in plugin so it registers itself (F03).
// Adding a plugin means adding one import here.
package all

import (
	_ "github.com/plusclouds/monitoring.server/plugins/camera"
	_ "github.com/plusclouds/monitoring.server/plugins/http"
	_ "github.com/plusclouds/monitoring.server/plugins/icmp"
	_ "github.com/plusclouds/monitoring.server/plugins/push"
	_ "github.com/plusclouds/monitoring.server/plugins/redfish"
	_ "github.com/plusclouds/monitoring.server/plugins/snmp"
	_ "github.com/plusclouds/monitoring.server/plugins/xapi"
)
