//go:build !darwin

// seatbelt_other.go 在非 macOS 平台提供空实现：host 后端暂不套 OS 沙箱，
// Linux 的 bubblewrap 对接留待后续；调用方应通过 build tag 选择真实现。
// Author: fxbin
package tools

// SeatbeltAvailable 非 macOS 恒为 false。
func SeatbeltAvailable() bool { return false }

// SeatbeltProfile 非 macOS 不生成 profile。
func SeatbeltProfile(options SeatbeltOptions) (string, error) {
	return "", errSeatbeltUnsupported
}

// WrapSeatbelt 非 macOS 不支持包装。
func WrapSeatbelt(profilePath string, argv []string) ([]string, error) {
	return nil, errSeatbeltUnsupported
}

// HostWriteModeFromPolicy 与 darwin 版本语义一致。
func HostWriteModeFromPolicy(writePolicy string) SeatbeltWriteMode {
	if writePolicy == "deny" {
		return SeatbeltWriteNone
	}
	return SeatbeltWriteWorkspace
}

// errSeatbeltUnsupported 表示当前平台无 seatbelt。
type seatbeltUnsupportedError struct{}

func (seatbeltUnsupportedError) Error() string {
	return "seatbelt is not supported on this platform"
}

var errSeatbeltUnsupported = seatbeltUnsupportedError{}
