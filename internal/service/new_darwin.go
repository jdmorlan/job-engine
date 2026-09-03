package service

// New returns the manager for this platform.
//
// Dispatched by build tag rather than a runtime switch, so that the launchd
// and systemd implementations only compile where their tooling exists and
// neither has to be stubbed out on the other's platform.
func New(c Component) (Manager, error) { return newLaunchd(c) }
