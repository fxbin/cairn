// terminal_capability_test 锁定终端能力覆盖与探测优先级。
//
// Author: fxbin
package tools

import "testing"

// TestOverrideBeatsDetect 锁定显式覆盖优先。
func TestOverrideBeatsDetect(t *testing.T) {
	detected := DetectTerminalCapabilities("xterm-kitty", "truecolor")
	overridden := OverrideTerminalCapabilities(DefaultTerminalCapabilities(), true, false, false)
	effective := overridden.Effective(detected)
	if !effective.Hyperlinks || effective.TrueColor || effective.Images {
		t.Fatalf("覆盖应优先: %+v", effective)
	}
	if !effective.Overridden {
		t.Fatal("应标记已覆盖")
	}
}

// TestDetectWithoutOverride 锁定无覆盖时用探测值。
func TestDetectWithoutOverride(t *testing.T) {
	detected := DetectTerminalCapabilities("xterm-kitty", "truecolor")
	effective := DefaultTerminalCapabilities().Effective(detected)
	if !effective.Images || !effective.TrueColor {
		t.Fatalf("探测值应生效: %+v", effective)
	}
	plain := DetectTerminalCapabilities("vt100", "")
	if plain.Images || plain.Hyperlinks {
		t.Fatalf("vt100 不应有高级能力: %+v", plain)
	}
}
