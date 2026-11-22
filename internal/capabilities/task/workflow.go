// [INPUT]: 依赖任务版本和冻结定义的制品引用。
// [OUTPUT]: 持久绑定一个待创建或已核验的工作流实例。
// [POS]: 任务先固定实例身份。应用层核验工作流创建事实后完成绑定。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件保存自主任务的工作流归属。
// Author: fxbin
package task

import "github.com/fxbin/cairn/internal/contract"

// WorkflowBinding 保留用户授权时的任务版本，以及唯一的实例和定义锚点。
type WorkflowBinding struct {
	WorkflowID         string       `json:"workflow_id"`
	DefinitionID       string       `json:"definition_id"`
	DefinitionRevision uint64       `json:"definition_revision"`
	DefinitionArtifact contract.Ref `json:"definition_artifact"`
	ConfigFingerprint  string       `json:"config_fingerprint"`
	AuthorizedRevision uint64       `json:"authorized_revision"`
	ReworkAttempts     int          `json:"rework_attempts"`
	Attached           bool         `json:"attached"`
}

// PrepareWorkflow 固定实例身份。应用层先持久化此状态再创建工作流。
func (current *Task) PrepareWorkflow(binding WorkflowBinding) error {
	if current.State != Active || current.WorkflowBinding != nil || current.Definition.AcceptanceDigest == "" {
		return ErrTransition
	}
	if binding.AuthorizedRevision != current.Revision || binding.Attached ||
		binding.WorkflowID == "" || binding.DefinitionID == "" || binding.DefinitionRevision == 0 ||
		binding.DefinitionArtifact.SHA256 == "" || binding.ConfigFingerprint == "" || binding.ReworkAttempts < 0 {
		return ErrConflict
	}
	current.WorkflowBinding = &binding
	return nil
}

// AttachWorkflow 结束准备阶段。调用方必须先核验对应的创建事件。
func (current *Task) AttachWorkflow() error {
	if current.State != Active || current.WorkflowBinding == nil || current.WorkflowBinding.Attached ||
		current.WorkflowBinding.AuthorizedRevision >= current.Revision {
		return ErrTransition
	}
	binding := *current.WorkflowBinding
	binding.Attached = true
	current.WorkflowBinding = &binding
	return nil
}
