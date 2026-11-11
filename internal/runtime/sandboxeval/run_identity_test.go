package sandboxeval

import "testing"

// fixedEvaluationID 是测试共用的评测身份串；派生函数只校验非空，64 位十六
// 进制形态仅为可读性。
const fixedEvaluationID = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

// TestSandboxRunIDDeterministicAndClosedInputs 锁定单次 Run 身份派生合同：
// 同一四元组恒同 ID、任一输入变化都改变身份，残缺输入与越界臂取值失败关闭。
func TestSandboxRunIDDeterministicAndClosedInputs(t *testing.T) {
	first, err := SandboxRunID(fixedEvaluationID, ArmBaseline, "TopicEye", 1)
	if err != nil {
		t.Fatalf("derive run id: %v", err)
	}
	second, err := SandboxRunID(fixedEvaluationID, ArmBaseline, "TopicEye", 1)
	if err != nil || first != second {
		t.Fatalf("same inputs must derive identical ids: %q vs %q (%v)", first, second, err)
	}
	mutations := map[string]func() (string, error){
		"variant arm": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmVariant, "TopicEye", 1)
		},
		"other task": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmBaseline, "TokenTracker", 1)
		},
		"other attempt": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmBaseline, "TopicEye", 2)
		},
		"other evaluation": func() (string, error) {
			return SandboxRunID("sha256:5555555555555555555555555555555555555555555555555555555555555555", ArmBaseline, "TopicEye", 1)
		},
	}
	seen := map[string]string{first: "base"}
	for name, derive := range mutations {
		derived, err := derive()
		if err != nil {
			t.Fatalf("%s must derive cleanly: %v", name, err)
		}
		if clash, repeated := seen[derived]; repeated {
			t.Fatalf("%s id collides with %q", name, clash)
		}
		seen[derived] = name
	}
	negative := map[string]func() (string, error){
		"empty evaluation": func() (string, error) { return SandboxRunID("", ArmBaseline, "TopicEye", 1) },
		"empty task": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmBaseline, "", 1)
		},
		"zero attempt": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmBaseline, "TopicEye", 0)
		},
		"negative attempt": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, ArmBaseline, "TopicEye", -3)
		},
		"arm outside lexicon": func() (string, error) {
			return SandboxRunID(fixedEvaluationID, SuiteArm("control"), "TopicEye", 1)
		},
	}
	for name, derive := range negative {
		if _, err := derive(); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
}
