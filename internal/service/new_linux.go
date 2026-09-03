package service

// New returns the manager for this platform.
func New(c Component) (Manager, error) { return newSystemd(c) }
