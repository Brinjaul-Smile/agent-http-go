package agenthttp

// RequestError 表示可以直接返回给 HTTP 调用方的请求错误。
// StatusCode 用来区分参数错误、依赖缺失、上游输出异常等场景。
type RequestError struct {
	Message    string
	StatusCode int
}

// NewRequestError 创建面向调用方的错误；statusCode 为 0 时默认按 400 处理。
func NewRequestError(message string, statusCode int) *RequestError {
	if statusCode == 0 {
		statusCode = 400
	}
	return &RequestError{
		Message:    message,
		StatusCode: statusCode,
	}
}

// Error 实现标准 error 接口。
func (e *RequestError) Error() string {
	return e.Message
}
