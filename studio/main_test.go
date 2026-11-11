// main_test 锁定 server 模式监听端口的解析口径：环境变量可覆盖，
// 非法取值回落到默认端口，不改变默认值本身。
//
// Author: fxbin
package main

import "testing"

// TestServerPortReadsEnvironment 锁定环境变量能覆盖监听端口。
func TestServerPortReadsEnvironment(t *testing.T) {
	t.Setenv(serverPortEnv, "9099")
	if got := serverPort(); got != 9099 {
		t.Fatalf("环境变量指定的端口应生效，得到 %d", got)
	}
}

// TestServerPortFallsBackOnInvalidValue 锁定非法取值回落默认端口。
func TestServerPortFallsBackOnInvalidValue(t *testing.T) {
	for _, raw := range []string{"", "  ", "abc", "0", "-1", "70000"} {
		t.Setenv(serverPortEnv, raw)
		if got := serverPort(); got != defaultServerPort {
			t.Fatalf("取值 %q 应回落到默认端口 %d，得到 %d", raw, defaultServerPort, got)
		}
	}
}

// TestServerPortDefaultWhenUnset 锁定未设置时的默认端口。
func TestServerPortDefaultWhenUnset(t *testing.T) {
	t.Setenv(serverPortEnv, "")
	if got := serverPort(); got != defaultServerPort {
		t.Fatalf("未设置时应使用默认端口 %d，得到 %d", defaultServerPort, got)
	}
}
