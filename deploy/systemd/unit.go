// Package systemd carries the systemd unit of the agent, so berthy setup installs exactly the file the repository ships.
package systemd

import _ "embed"

// Unit is the content of berthy.service.
//
//go:embed berthy.service
var Unit []byte
