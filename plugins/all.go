// Package plugins links every analysis plugin into the binary. To add a
// plugin, create a package that calls plugin.Register in init() and import it
// here.
package plugins

import (
	_ "agent-security/plugins/audiostats"
)
