package errs

import "errors"

var (
	ErrNotFound = errors.New("resource not found")
	ErrDuplicate = errors.New("resource already exists")
	ErrValidation = errors.New("validation error")
)