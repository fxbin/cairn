// 本文件定义 Provider 持久化记录的安全错误包装，保留程序化错误链但不把
// 不可信磁盘内容、文件名或本机路径暴露给 CLI、TUI 与日志。
//
// Author: fxbin
package provider

// persistedRecordError 用固定消息描述持久化记录错误；cause 仅供 errors.Is/As
// 程序化检查，Error 不拼接底层可能含敏感值的文本。
type persistedRecordError struct {
	message string
	cause   error
}

// Error 返回不含持久化内容和路径的稳定错误消息。
func (err *persistedRecordError) Error() string {
	return err.message
}

// Unwrap 暴露底层错误身份，供调用方分类但不用于用户可见文本。
func (err *persistedRecordError) Unwrap() error {
	return err.cause
}

// newPersistedRecordError 构造保留底层错误身份的安全持久化错误。
func newPersistedRecordError(message string, cause error) error {
	return &persistedRecordError{message: message, cause: cause}
}
