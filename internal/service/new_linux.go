package service

// New returns the manager for this platform.
func New() (Manager, error) { return newSystemd() }
