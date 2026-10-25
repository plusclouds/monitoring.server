// Package all imports every built-in collector so it registers itself.
// Adding a collector means adding one import here.
package all

import (
	_ "github.com/plusclouds/monitoring.server/internal/agent/collectors/docker"
	_ "github.com/plusclouds/monitoring.server/internal/agent/collectors/host"
	_ "github.com/plusclouds/monitoring.server/internal/agent/collectors/probe"
	_ "github.com/plusclouds/monitoring.server/internal/agent/collectors/services"
)
