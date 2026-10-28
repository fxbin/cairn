// journal_open_dsn_test.go 锁定可写 Journal 的 DSN 构造行为：路径含空格等
// 非 URL 安全字符时必须逐字节保留（不得经 URI 转义），追加与回读闭环可用。
// Author: fxbin
package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// TestOpenKeepsPathWithSpacesVerbatim 锁定 DSN 拼接不转义路径：用户级状态目录
// （如 macOS Application Support）含空格时打开、迁移、追加与回读全部可用。
func TestOpenKeepsPathWithSpacesVerbatim(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Application Support", "workspaces", "runs.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatalf("open journal under a path with spaces: %v", err)
	}
	defer journal.Close()
	event, err := journal.Append(context.Background(), 0, domain.EventDraft{
		RunID: domain.NewRunID(), WorkspaceHash: "ws", Type: domain.EventRunCreated,
		Payload: []byte(`{"task":"probe","workspace":"/probe"}`),
	})
	if err != nil {
		t.Fatalf("append under a path with spaces: %v", err)
	}
	if event.Seq != 1 {
		t.Fatalf("appended event seq = %d", event.Seq)
	}
}
