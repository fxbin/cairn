// delegate_task_async_test.go 锁定 delegate_task 的 async 参数声明与解析：
// schema 暴露布尔 async，缺省行为与同步委派完全一致。
// Author: fxbin
package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDelegateTaskSpecExposesAsyncParameter 锁定模型可见 schema 含 async 布尔
// 属性且描述说明需配置显式启用。
func TestDelegateTaskSpecExposesAsyncParameter(t *testing.T) {
	spec := DelegateTask{}.Spec()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties["async"]) == 0 {
		t.Fatalf("delegate_task schema must expose async: %s", spec.Parameters)
	}
	if !strings.Contains(spec.Description, "async=true") || !strings.Contains(spec.Description, "delegate_async") {
		t.Fatalf("delegate_task description must explain the async flag: %s", spec.Description)
	}
}

// TestDelegateRequestParsesAsyncFlag 锁定请求解析：async 缺省 false，显式 true
// 可解析。
func TestDelegateRequestParsesAsyncFlag(t *testing.T) {
	var syncRequest DelegateRequest
	if err := json.Unmarshal([]byte(`{"task":"child work"}`), &syncRequest); err != nil {
		t.Fatal(err)
	}
	if syncRequest.Async {
		t.Fatal("default delegation must stay synchronous")
	}
	var asyncRequest DelegateRequest
	if err := json.Unmarshal([]byte(`{"task":"child work","async":true}`), &asyncRequest); err != nil {
		t.Fatal(err)
	}
	if !asyncRequest.Async || asyncRequest.Task != "child work" {
		t.Fatalf("async request = %+v", asyncRequest)
	}
}
