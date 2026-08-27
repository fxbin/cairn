package sandboxeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// LoadCredentialLease 从本地忽略目录中的租约描述文件装载并校验租约：JSON
// 严格解码拒绝未知字段与尾随内容，schema 版本与全部合同约束在解析后立即复
// 核。该函数只读取不含密钥的描述结构；Key 本体由可信调用方经内存注入，永
// 远不落盘。
func LoadCredentialLease(path string) (CredentialLease, error) {
	var lease CredentialLease
	if err := decodeFrozenJSONFile(path, &lease); err != nil {
		return CredentialLease{}, fmt.Errorf("credential lease: %w", err)
	}
	if err := lease.Validate(); err != nil {
		return CredentialLease{}, fmt.Errorf("credential lease %q rejected: %w", path, err)
	}
	return lease, nil
}

// LoadCredentialPolicy 从本地忽略目录中的冻结策略文件装载并校验凭据策略：
// 解码入口与租约共用同一严格实现，指纹只可由本结构计算。
func LoadCredentialPolicy(path string) (CredentialPolicy, error) {
	var policy CredentialPolicy
	if err := decodeFrozenJSONFile(path, &policy); err != nil {
		return CredentialPolicy{}, fmt.Errorf("credential policy: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return CredentialPolicy{}, fmt.Errorf("credential policy %q rejected: %w", path, err)
	}
	return policy, nil
}

// decodeFrozenJSONFile 是全部冻结输入装载的唯一解码实现：单文档、禁未知字
// 段、拒绝尾随内容，任何半解析状态都不得进入评测决策。
func decodeFrozenJSONFile(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read frozen input file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %q: file must contain exactly one JSON document", path)
		}
		return fmt.Errorf("decode %q: %w", path, err)
	}
	return nil
}
