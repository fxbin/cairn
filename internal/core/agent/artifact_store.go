// artifact_store.go 定义 Loop 使用的最小制品能力端口：Core 只依赖合同层的
// 引用类型与这组方法，由应用层注入 Runtime 的制品存储实现。
//
// Author: fxbin
package agent

import "github.com/fxbin/cairn/internal/contract"

// ArtifactStore 是 Loop 写入与读取内容寻址制品所需的最小能力。OpenBounded
// 供项目规约快照等路径按引用有界读取，避免先整体分配再校验大小。
type ArtifactStore interface {
	Put(content []byte, mediaType string) (contract.Ref, error)
	Open(ref contract.Ref) ([]byte, error)
	OpenBounded(ref contract.Ref, maximumBytes int64) ([]byte, error)
}
