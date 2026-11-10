// skill_content.go 定义技能加载成功后可写入 Journal 的版本化内容合同；
// 字段顺序与 JSON 名称绑定历史回执，迁移前后序列化字节必须逐位一致。
// 内容不包含宿主机目录，也不能授予权限。
// Author: fxbin
package contract

// SkillContentVersion 是技能内容快照的当前合同版本。
const SkillContentVersion = 1

// SkillResource 绑定一个普通资源文件的相对位置、长度与原始摘要。
type SkillResource struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// SkillContent 是成功加载后可写入 Journal 的版本化内容，不包含宿主机目录。
type SkillContent struct {
	Version       int             `json:"version"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Compatibility string          `json:"compatibility,omitempty"`
	Scope         string          `json:"scope"`
	SourceDigest  string          `json:"source_digest"`
	SHA256        string          `json:"sha256"`
	Instructions  string          `json:"instructions"`
	Resources     []SkillResource `json:"resources"`
}
