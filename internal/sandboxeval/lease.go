package sandboxeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// LoadCredentialLease 从本地忽略目录中的租约描述文件装载并校验租约：JSON
// 严格解码拒绝未知字段，schema 版本与全部合同约束在解析后立即复核。该函数
// 只读取不含密钥的描述结构；Key 本体由可信调用方经内存注入，永远不落盘。
func LoadCredentialLease(path string) (CredentialLease, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return CredentialLease{}, fmt.Errorf("read credential lease file: %w", err)
	}
	var lease CredentialLease
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lease); err != nil {
		return CredentialLease{}, fmt.Errorf("decode credential lease %q: %w", path, err)
	}
	if err := lease.Validate(); err != nil {
		return CredentialLease{}, fmt.Errorf("credential lease %q rejected: %w", path, err)
	}
	return lease, nil
}
