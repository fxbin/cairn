package tools

import "errors"

// errOSSandboxUnavailable 表示当前平台缺少 OS 级 host 约束能力。
var errOSSandboxUnavailable = errors.New("OS sandbox unavailable for host confinement")
