// Package app 在新运行创建前冻结用户与工作区 AGENTS.md；恢复只消费创建事件中的制品引用。
// Author: fxbin
package app

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/fxbin/cairn/internal/userdir"
)

// freezeProjectInstructions 保存已脱敏的完整项目规约；没有有效文档时不生成引用，不改变旧运行合同。
func freezeProjectInstructions(workspace tools.Workspace, store *artifact.Store, disableUser bool) (*artifact.Ref, error) {
	userDirectory := ""
	if !disableUser {
		if directory, err := userdir.Root(); err == nil {
			userDirectory = directory
		}
	}

	snapshot, err := tools.LoadProjectInstructionsWithUser(workspace, userDirectory)
	if err != nil {
		return nil, fmt.Errorf("load AGENTS.md project instructions: %w", err)
	}
	if len(snapshot.Documents) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, errors.New("encode project instructions snapshot failed")
	}
	if len(encoded) > tools.ProjectInstructionsMaxSnapshotBytes || store == nil {
		return nil, errors.New("project instructions snapshot storage limit exceeded or unavailable")
	}
	reference, err := store.Put(encoded, "application/json")
	if err != nil {
		return nil, errors.New("store project instructions snapshot failed")
	}
	return &reference, nil
}
