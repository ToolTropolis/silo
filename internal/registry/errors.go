package registry

import "errors"

var errNotImplemented = errors.New("registry: not implemented")

// ErrNotFound is returned by Get when no project record exists.
var ErrNotFound = errors.New("registry: project not found")
