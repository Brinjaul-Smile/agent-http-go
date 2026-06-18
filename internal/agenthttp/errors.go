package agenthttp

import (
	"errors"
	"fmt"
)

type RequestError struct {
	Message    string
	StatusCode int
}

func NewRequestError(message string, statusCode int) *RequestError {
	if statusCode == 0 {
		statusCode = 400
	}
	return &RequestError{
		Message:    message,
		StatusCode: statusCode,
	}
}

func (e *RequestError) Error() string {
	return e.Message
}

func AsRequestError(err error, target **RequestError) bool {
	return errors.As(err, target)
}

func requestErrorf(statusCode int, format string, args ...any) *RequestError {
	return NewRequestError(fmt.Sprintf(format, args...), statusCode)
}
