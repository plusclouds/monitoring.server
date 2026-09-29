// Package all imports every built-in plugin so it registers itself (F03).
// Adding a plugin means adding one import here.
package all

import (
	_ "github.com/plusclouds/monitoring.server/plugins/http"
	_ "github.com/plusclouds/monitoring.server/plugins/icmp"
)
