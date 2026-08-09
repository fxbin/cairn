// Package artifact 提供经脱敏的内容寻址证据存储；制品只用于留证，不是密钥保管库。
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Ref 是可持久化的制品引用及供用户查看的截断预览。
type Ref struct {
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	MediaType        string `json:"media_type"`
	TruncatedPreview string `json:"truncated_preview"`
}

// Store 管理本地制品目录、预览上限和单个制品大小上限。
type Store struct {
	Root        string
	PreviewSize int
	MaxBytes    int
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer)\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password)\s*[:=]\s*[^\s,"'}]+`),
	regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{12,}`),
}

// New 创建仅当前用户可读写的制品根目录。
func New(root string, previewSize int) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("artifact root is required")
	}
	if previewSize <= 0 {
		previewSize = 16 * 1024
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure artifact root: %w", err)
	}
	return &Store{Root: root, PreviewSize: previewSize, MaxBytes: 10 * 1024 * 1024}, nil
}

// Redact 按已知凭据模式替换文本中可能泄露的敏感值。
func Redact(content []byte) []byte {
	clean := string(content)
	for _, pattern := range secretPatterns {
		clean = pattern.ReplaceAllStringFunc(clean, func(match string) string {
			if strings.HasPrefix(strings.ToLower(match), "bearer ") {
				return "Bearer [REDACTED]"
			}
			if index := strings.IndexAny(match, ":="); index >= 0 {
				return match[:index+1] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return []byte(clean)
}

// RedactJSON 优先保留有效 JSON 结构并清除凭据式键的值，再执行文本脱敏。
func RedactJSON(content []byte) []byte {
	var value any
	if err := json.Unmarshal(content, &value); err != nil {
		return Redact(content)
	}
	scrubJSON(&value, "")
	encoded, err := json.Marshal(value)
	if err != nil {
		return Redact(content)
	}
	return Redact(encoded)
}

// scrubJSON 递归替换敏感键及字符串叶子节点中的凭据内容。
func scrubJSON(value *any, key string) {
	if sensitiveKey(key) {
		*value = "[REDACTED]"
		return
	}
	switch current := (*value).(type) {
	case map[string]any:
		for nestedKey, nested := range current {
			scrubJSON(&nested, nestedKey)
			current[nestedKey] = nested
		}
	case []any:
		for index, nested := range current {
			scrubJSON(&nested, "")
			current[index] = nested
		}
	case string:
		*value = string(Redact([]byte(current)))
	}
}

// sensitiveKey 判断规范化后的 JSON 键是否可能承载凭据；用量计数和预算上限
// 虽然包含 tokens 字样，但属于审计与预算事实，必须保留。
func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return key == "token" || key == "apikey" || key == "api_key" ||
		(strings.HasSuffix(key, "token") && !strings.HasSuffix(key, "tokens")) ||
		strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "credential")
}

// Put 脱敏后按内容哈希原子写入制品，并返回稳定引用。
func (store *Store) Put(content []byte, mediaType string) (Ref, error) {
	redacted := Redact(content)
	if store.MaxBytes > 0 && len(redacted) > store.MaxBytes {
		return Ref{}, fmt.Errorf("artifact exceeds configured maximum of %d bytes", store.MaxBytes)
	}
	digest := sha256.Sum256(redacted)
	hexDigest := hex.EncodeToString(digest[:])
	directory := filepath.Join(store.Root, "sha256", hexDigest[:2])
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Ref{}, fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return Ref{}, fmt.Errorf("secure artifact directory: %w", err)
	}
	path := filepath.Join(directory, hexDigest)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, redacted) {
			return Ref{}, fmt.Errorf("artifact hash collision at %s", path)
		}
		return store.ref(hexDigest, int64(len(redacted)), mediaType, redacted), nil
	} else if !os.IsNotExist(err) {
		return Ref{}, fmt.Errorf("read existing artifact: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".cairn-artifact-*")
	if err != nil {
		return Ref{}, fmt.Errorf("create artifact temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(redacted)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Ref{}, fmt.Errorf("write artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if !os.IsExist(err) {
			return Ref{}, fmt.Errorf("publish artifact: %w", err)
		}
	} else if err := syncDirectory(directory); err != nil {
		return Ref{}, err
	}
	return store.ref(hexDigest, int64(len(redacted)), mediaType, redacted), nil
}

// ref 由已写入的内容生成带长度受限预览的制品引用。
func (store *Store) ref(digest string, bytes int64, mediaType string, content []byte) Ref {
	preview := content
	if len(preview) > store.PreviewSize {
		preview = append(append([]byte(nil), preview[:store.PreviewSize]...), []byte("…")...)
	}
	return Ref{SHA256: "sha256:" + digest, Bytes: bytes, MediaType: mediaType, TruncatedPreview: string(preview)}
}

// syncDirectory 同步目录元数据，使原子重命名后的制品更可靠地落盘。
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open artifact directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}
