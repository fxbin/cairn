// Package artifact 提供经脱敏的内容寻址证据存储；制品只用于留证，不是密钥保管库。
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fxbin/cairn/internal/credential"
)

// Ref 是可持久化的制品引用及供用户查看的截断预览。
type Ref struct {
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	MediaType        string `json:"media_type"`
	TruncatedPreview string `json:"truncated_preview"`
}

// Open 读取并校验一个已存在制品：内容 SHA-256 必须与引用一致，否则失败
// 关闭——制品引用的完整性承诺对读取侧与写入侧同等生效。不存在的引用返
// 回错误，由调用方决定降级语义。
func (store *Store) Open(ref Ref) ([]byte, error) {
	return store.open(ref, 0)
}

// OpenBounded 在分配正文前约束实体大小，并对读取中增长的文件继续限额；既有 Open 的不限额读取语义不变。
func (store *Store) OpenBounded(ref Ref, maximumBytes int64) ([]byte, error) {
	if maximumBytes < 1 || maximumBytes == math.MaxInt64 || ref.Bytes < 0 || ref.Bytes > maximumBytes {
		return nil, errors.New("invalid artifact read limit or reference size")
	}
	return store.open(ref, maximumBytes)
}

// open 共享内容寻址和摘要校验，只有调用方显式给出上限时才采用有界读取。
func (store *Store) open(ref Ref, maximumBytes int64) ([]byte, error) {
	if ref.SHA256 == "" {
		return nil, errors.New("artifact ref without digest")
	}
	hexDigest := strings.TrimPrefix(ref.SHA256, "sha256:")
	if len(hexDigest) != sha256.Size*2 {
		return nil, fmt.Errorf("artifact ref digest has unexpected length")
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return nil, errors.New("artifact ref digest is not hexadecimal")
	}
	path := filepath.Join(store.Root, "sha256", hexDigest[:2], hexDigest)
	var content []byte
	var err error
	if maximumBytes > 0 {
		content, err = readBoundedArtifact(path, maximumBytes)
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != hexDigest {
		return nil, fmt.Errorf("artifact content hash mismatch at %s", path)
	}
	return content, nil
}

// readBoundedArtifact 拒绝非普通实体和已知超限大小，再用有限读取防止检查后的文件增长耗尽内存。
func readBoundedArtifact(path string, maximumBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("bounded artifact must be a regular file")
	}
	if info.Size() > maximumBytes {
		return nil, errors.New("artifact exceeds read limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(content)) > maximumBytes {
		return nil, errors.New("artifact exceeds read limit")
	}
	return content, nil
}

// Store 管理本地制品目录、预览上限和单个制品大小上限。
type Store struct {
	Root        string
	PreviewSize int
	MaxBytes    int
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer)\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{12,}`),
}

// credentialAssignmentPattern 捕获带引号或足够长裸值的候选赋值；闭合引号值
// 保留既有长度阈值，未闭合到 EOF 的非空片段也脱敏，防止输出截断后漏值。
// 键名敏感性统一交给 credential.Key，避免文本、JSON、配置和工具各自维护词干。
var credentialAssignmentPattern = regexp.MustCompile(`(?i)\b[A-Za-z_][A-Za-z0-9_-]*["']?\s*[:=]\s*(?:"[^"]{4,}"|'[^']{4,}'|"[^"]+$|'[^']+$|[A-Za-z0-9+/=_-]{16,})`)

// credentialEnvAssignmentPattern 只捕获行首的“环境变量=值”赋值（.env 与 shell
// 导出风格）。与上面的通用赋值模式不同，它行锚定且不限值长度：短的
// DB_PASSWORD=hunter2 同样必须清除，而代码中带空格的比较或赋值
// （password == ""、token := x）不会落在行首裸赋值形态上而被误伤。
var credentialEnvAssignmentPattern = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)=([^[:space:]"']+)`)

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
	clean = credentialAssignmentPattern.ReplaceAllStringFunc(clean, func(match string) string {
		index := strings.IndexAny(match, ":=")
		if index < 0 {
			return match
		}
		key := strings.Trim(strings.TrimSpace(match[:index]), "\"'")
		if !credential.Key(key) {
			return match
		}
		return match[:index+1] + "[REDACTED]"
	})
	clean = credentialEnvAssignmentPattern.ReplaceAllStringFunc(clean, func(match string) string {
		index := strings.Index(match, "=")
		if index < 0 || !credential.Key(match[:index]) {
			return match
		}
		return match[:index+1] + "[REDACTED]"
	})
	return []byte(clean)
}

// RedactJSON 保留有效 JSON 结构和数字精度，再递归清除凭据式键及字符串内容。
// EncodeRedactedJSON 将值序列化为脱敏后的事件载荷 JSON。编码失败表示调用方
// 构造了不可序列化值，属于编程错误，直接 panic，禁止把未脱敏残片写入日志。
// EncodeRedactedJSON 序列化并脱敏载荷；失败返回错误，不 panic，避免进程
// 因单条载荷不可编码而崩溃并留下半截运行。
func EncodeRedactedJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode event payload: %w", err)
	}
	return append(json.RawMessage(nil), RedactJSON(encoded)...), nil
}

// MustEncodeRedactedJSON 是测试与脚本化计划使用的便捷包装；生产路径应
// 优先使用 EncodeRedactedJSON 并处理错误。
func MustEncodeRedactedJSON(value any) json.RawMessage {
	raw, err := EncodeRedactedJSON(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func RedactJSON(content []byte) []byte {
	if !json.Valid(content) {
		return Redact(content)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Redact(content)
	}
	scrubJSON(&value, "")
	encoded, err := json.Marshal(value)
	if err != nil {
		return Redact(content)
	}
	// 已完成递归脱敏的 value 重新编码后必定仍是 JSON。此处不能再把它交给
	// 面向任意文本的 Redact：源码字符串中的 credential-like 片段可能含有
	// 反斜杠，文本替换会破坏 JSON 转义并使后续事件无法持久化。
	return encoded
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
	return !strings.HasSuffix(key, "tokens") && credential.Key(key)
}

// Put 脱敏后按内容哈希原子写入制品，并返回稳定引用；JSON 制品优先走保留
// 结构的递归键脱敏，使内嵌于字符串叶子中的凭据同样被清除。
func (store *Store) Put(content []byte, mediaType string) (Ref, error) {
	redacted := Redact(content)
	if mediaType == "application/json" {
		redacted = RedactJSON(content)
	}
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
	if err := publishArtifact(temporary, path, redacted); err != nil {
		return Ref{}, err
	}
	return store.ref(hexDigest, int64(len(redacted)), mediaType, redacted), nil
}

// artifactTemporaryFile 限定制品发布所需的临时文件操作，生产实现由 os.File 提供。
type artifactTemporaryFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// publishArtifact 仅在临时制品写入、同步与关闭成功后发布，并清理未发布的临时文件。
func publishArtifact(temporary artifactTemporaryFile, path string, content []byte) error {
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	err := temporary.Chmod(0o600)
	if err == nil {
		var written int
		written, err = temporary.Write(content)
		if err == nil && written != len(content) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("publish artifact: %w", err)
		}
	} else if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
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
