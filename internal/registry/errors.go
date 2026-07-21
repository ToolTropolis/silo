package registry

import "errors"

// ErrNotFound is returned when no project record matches.
var ErrNotFound = errors.New("registry: project not found")

// ErrAlreadyExists is returned by Register when the project is already present.
var ErrAlreadyExists = errors.New("registry: project already exists")
