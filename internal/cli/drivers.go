package cli

// drivers.go — THE one place the binary links the Go drivers. Each driver
// registers itself in its init() (driver.Register); nothing else imports
// the driver packages, so a command that reaches the registry — the
// provision dispatch (up/provision/destroy/down/status/bootstrap), `lo
// drivers`, `lo kubehz` — finds every driver only because of these blank
// imports. Keep the set complete: a driver missing here is "Unknown cluster
// kind" at dispatch time, with no compile error to warn anyone.

import (
	_ "github.com/kernpilot/lok8s/internal/driver/capi"
	_ "github.com/kernpilot/lok8s/internal/driver/kkp"
	_ "github.com/kernpilot/lok8s/internal/driver/kubehz"
	_ "github.com/kernpilot/lok8s/internal/driver/kubeone"
	_ "github.com/kernpilot/lok8s/internal/driver/lo"
)
