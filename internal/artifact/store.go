// Package artifact 提供经脱敏的内容寻址证据存储；制品只用于留证，不是密钥保管库。
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// Open 读取并校验一个已存在制品：内容 SHA-256 必须与引用一致，否则失败
// 关闭——制品引用的完整性承诺对读取侧与写入侧同等生效。不存在的引用返
// 回错误，由调用方决定降级语义。
func (store *Store) Open(ref contract.Ref) ([]byte, error) {
	return store.open(ref, 0)
}

// OpenBounded 在分配正文前约束实体大小，并对读取中增长的文件继续限额；既有 Open 的不限额读取语义不变。
func (store *Store) OpenBounded(ref contract.Ref, maximumBytes int64) ([]byte, error) {
	if maximumBytes < 1 || maximumBytes == math.MaxInt64 || ref.Bytes < 0 || ref.Bytes > maximumBytes {
		return nil, errors.New("invalid artifact read limit or reference size")
	}
	return store.open(ref, maximumBytes)
}

// open 共享内容寻址和摘要校验，只有调用方显式给出上限时才采用有界读取。
func (store *Store) open(ref contract.Ref, maximumBytes int64) ([]byte, error) {
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

// Put 脱敏后按内容哈希原子写入制品，并返回稳定引用；JSON 制品优先走保留
// 结构的递归键脱敏，使内嵌于字符串叶子中的凭据同样被清除。
func (store *Store) Put(content []byte, mediaType string) (contract.Ref, error) {
	redacted := contract.Redact(content)
	if mediaType == "application/json" {
		redacted = contract.RedactJSON(content)
	}
	if store.MaxBytes > 0 && len(redacted) > store.MaxBytes {
		return contract.Ref{}, fmt.Errorf("artifact exceeds configured maximum of %d bytes", store.MaxBytes)
	}
	digest := sha256.Sum256(redacted)
	hexDigest := hex.EncodeToString(digest[:])
	directory := filepath.Join(store.Root, "sha256", hexDigest[:2])
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return contract.Ref{}, fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return contract.Ref{}, fmt.Errorf("secure artifact directory: %w", err)
	}
	path := filepath.Join(directory, hexDigest)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, redacted) {
			return contract.Ref{}, fmt.Errorf("artifact hash collision at %s", path)
		}
		return store.ref(hexDigest, int64(len(redacted)), mediaType, redacted), nil
	} else if !os.IsNotExist(err) {
		return contract.Ref{}, fmt.Errorf("read existing artifact: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".cairn-artifact-*")
	if err != nil {
		return contract.Ref{}, fmt.Errorf("create artifact temp file: %w", err)
	}
	if err := publishArtifact(temporary, path, redacted); err != nil {
		return contract.Ref{}, err
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
func (store *Store) ref(digest string, bytes int64, mediaType string, content []byte) contract.Ref {
	preview := content
	if len(preview) > store.PreviewSize {
		preview = append(append([]byte(nil), preview[:store.PreviewSize]...), []byte("…")...)
	}
	return contract.Ref{SHA256: "sha256:" + digest, Bytes: bytes, MediaType: mediaType, TruncatedPreview: string(preview)}
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
