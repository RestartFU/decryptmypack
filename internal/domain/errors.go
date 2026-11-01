package domain

import "errors"

var (
	ErrHostIpListEmpty    = errors.New("host ip list is empty")
	ErrPortNotValidNumber = errors.New("port is not a valid number")
	ErrHostInvalid        = errors.New("host is invalid")
	ErrCouldNotUploadPack = errors.New("could not upload pack")
)
