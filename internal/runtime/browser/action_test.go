// action_test 锁定 BRW-04 受控页面操作的风险分级与目标绑定。
//
// Author: fxbin
package browser

import "testing"

// TestValidatePageAction 锁定必填字段与类型校验。
func TestValidatePageAction(t *testing.T) {
	ok := PageAction{ID: "a1", TabID: "tab-1", Selector: "#btn", Type: ActionClick}
	if err := ValidatePageAction(ok); err != nil {
		t.Fatalf("合法操作应通过: %v", err)
	}
	cases := []PageAction{
		{ID: "", TabID: "t", Selector: "#x", Type: ActionClick},
		{ID: "a", TabID: "", Selector: "#x", Type: ActionClick},
		{ID: "a", TabID: "t", Selector: "", Type: ActionClick},
		{ID: "a", TabID: "t", Selector: "#x", Type: "unknown"},
		{ID: "a", TabID: "t", Selector: "#x", Type: ActionInput, Value: ""},
	}
	for _, action := range cases {
		if err := ValidatePageAction(action); err == nil {
			t.Fatalf("应拒绝 %+v", action)
		}
	}
}

// TestClassifyRisk 锁定上传与敏感动作用 high。
func TestClassifyRisk(t *testing.T) {
	if ClassifyRisk(ActionUpload, "") != RiskHigh {
		t.Fatal("上传应为 high")
	}
	if ClassifyRisk(ActionInput, "password=secret") != RiskHigh {
		t.Fatal("敏感输入应为 high")
	}
	if ClassifyRisk(ActionClick, "") != RiskLow {
		t.Fatal("点击应为 low")
	}
}
