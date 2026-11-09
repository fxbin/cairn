// artifact_ref.go 定义内容寻址制品的持久化引用合同：事件载荷与制品存储共用
// 同一字段顺序和 JSON 名称，保证迁移前后序列化字节不变。
//
// Author: fxbin
package contract

// Ref 是可持久化的制品引用及供用户查看的截断预览。
type Ref struct {
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	MediaType        string `json:"media_type"`
	TruncatedPreview string `json:"truncated_preview"`
}
