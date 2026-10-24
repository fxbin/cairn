package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
)

// 本文件实现桌面端异步运行控制器：初始化快照 + 增量事件，以 runID 会话隔离，
// 旧回调不得污染新会话。Event Journal 仍是事实源，这里只做通知与唤醒。

// Emitter 向前端推送 StreamEvent；由 Wails 层实现。
type Emitter func(StreamEvent)

// PrefsStore 读写 UI 偏好（最近项目、会话元数据）。偏好不进入 Journal。
type PrefsStore struct {
	mu      sync.RWMutex
	path    string
	data    Preferences
	loadErr error
}

// Preferences 是 GUI 本地偏好。
type Preferences struct {
	RecentProjects []RecentProject      `json:"recent_projects"`
	SessionMeta    []SessionMeta        `json:"session_meta"`
	RunOverrides   []RunOverride        `json:"run_overrides,omitempty"`
	Hooks          []HookRule           `json:"hooks,omitempty"`
	Workflows      []WorkflowDefinition `json:"workflows,omitempty"`
	ReviewNotes    []ReviewNote         `json:"review_notes,omitempty"`
	Theme          string               `json:"theme,omitempty"`
	FontSize       int                  `json:"font_size,omitempty"`
}

// RunOverride 保存启动时的运行时覆盖，使审批恢复可复现同一冻结指纹。
type RunOverride struct {
	RunID string `json:"run_id"`
	Model string `json:"model,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// HookAction 是钩子允许的有限动作之一。
type HookAction struct {
	// Kind 仅支持 queue_followup（入队后续任务）。
	Kind    string `json:"kind"`
	Content string `json:"content,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// HookRule 绑定工作区与事件到有限动作；不能提升权限。
type HookRule struct {
	ID        string       `json:"id"`
	Workspace string       `json:"workspace"`
	Event     string       `json:"event"`
	Enabled   bool         `json:"enabled"`
	Actions   []HookAction `json:"actions"`
	// FailStreak 连续失败次数；达到阈值自动停用。
	FailStreak int `json:"fail_streak,omitempty"`
}

// SaveRunOverride 记录 run 的运行时覆盖。
func (store *PrefsStore) SaveRunOverride(override RunOverride) error {
	if strings.TrimSpace(override.RunID) == "" {
		return errInvalid("run_id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.RunOverrides {
		if store.data.RunOverrides[i].RunID == override.RunID {
			store.data.RunOverrides[i] = override
			return store.save()
		}
	}
	store.data.RunOverrides = append(store.data.RunOverrides, override)
	const maxOverrides = 200
	if len(store.data.RunOverrides) > maxOverrides {
		store.data.RunOverrides = store.data.RunOverrides[len(store.data.RunOverrides)-maxOverrides:]
	}
	return store.save()
}

// LoadRunOverride 读取 run 的运行时覆盖；不存在时返回空值。
func (store *PrefsStore) LoadRunOverride(runID string) (RunOverride, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, item := range store.data.RunOverrides {
		if item.RunID == runID {
			return item, true
		}
	}
	return RunOverride{}, false
}

// NewPrefsStore 在用户配置根下打开偏好文件（不存在则空）。
func NewPrefsStore() (*PrefsStore, error) {
	root, err := userConfigRoot()
	if err != nil {
		return nil, err
	}
	store := &PrefsStore{path: filepath.Join(root, preferencesFile)}
	store.loadErr = store.load()
	return store, nil
}

// NewPrefsStoreAt 供测试指定偏好文件路径。
func NewPrefsStoreAt(path string) *PrefsStore {
	store := &PrefsStore{path: path}
	// 损坏文件 load 返回错误但不阻止进程启动；后续保存会再次失败关闭，
	// 且损坏内容已旁路保全。测试与调用方可读取 store 状态。
	store.loadErr = store.load()
	return store
}

func (store *PrefsStore) load() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			store.data = Preferences{RecentProjects: []RecentProject{}, SessionMeta: []SessionMeta{}}
			return nil
		}
		return err
	}
	var decoded Preferences
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// 损坏偏好保全到旁路文件，避免后续保存静默覆盖用户数据。
		store.data = Preferences{RecentProjects: []RecentProject{}, SessionMeta: []SessionMeta{}}
		backup, backupErr := os.OpenFile(store.path+".corrupt", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(backupErr, os.ErrExist) {
			backup, backupErr = os.CreateTemp(filepath.Dir(store.path), filepath.Base(store.path)+".corrupt-*")
		}
		if backupErr != nil {
			return errInvalid("preferences file is corrupt; backup unavailable; original retained")
		}
		_, writeErr := backup.Write(raw)
		if err := errors.Join(writeErr, backup.Sync(), backup.Close()); err != nil {
			return errInvalid("preferences file is corrupt; backup incomplete; original retained")
		}
		return errInvalid("preferences file is corrupt; preserved at " + backup.Name())
	}
	if decoded.RecentProjects == nil {
		decoded.RecentProjects = []RecentProject{}
	}
	if decoded.SessionMeta == nil {
		decoded.SessionMeta = []SessionMeta{}
	}
	store.data = decoded
	return nil
}

// save 使用独占临时文件同步并替换偏好，避免固定临时路径跟随链接。
func (store *PrefsStore) save() error {
	if store.loadErr != nil {
		return store.loadErr
	}
	raw, err := json.MarshalIndent(store.data, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".preferences-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), store.path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

// ListRecentProjects 返回最近项目（默认不含已归档）。
func (store *PrefsStore) ListRecentProjects(includeArchived bool) []RecentProject {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]RecentProject, 0, len(store.data.RecentProjects))
	for _, item := range store.data.RecentProjects {
		if item.Archived && !includeArchived {
			continue
		}
		out = append(out, item)
	}
	return out
}

// TouchProject 记录项目使用并置顶到最近列表。
func (store *PrefsStore) TouchProject(path string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	found := -1
	for i := range store.data.RecentProjects {
		if store.data.RecentProjects[i].Path == abs {
			found = i
			break
		}
	}
	item := RecentProject{Path: abs, Name: filepath.Base(abs), LastUsed: time.Now().UTC()}
	if found >= 0 {
		item.Pinned = store.data.RecentProjects[found].Pinned
		item.Archived = store.data.RecentProjects[found].Archived
		store.data.RecentProjects = append(store.data.RecentProjects[:found], store.data.RecentProjects[found+1:]...)
	}
	store.data.RecentProjects = append([]RecentProject{item}, store.data.RecentProjects...)
	const maxRecent = 30
	if len(store.data.RecentProjects) > maxRecent {
		store.data.RecentProjects = store.data.RecentProjects[:maxRecent]
	}
	return store.save()
}

// SetProjectArchived 归档或恢复最近项目展示（不删除运行历史）。
func (store *PrefsStore) SetProjectArchived(path string, archived bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for i := range store.data.RecentProjects {
		if store.data.RecentProjects[i].Path == abs {
			store.data.RecentProjects[i].Archived = archived
			return store.save()
		}
	}
	return os.ErrNotExist
}

// SessionPreferences 返回会话展示元数据。
func (store *PrefsStore) SessionPreferences() []SessionMeta {
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make([]SessionMeta, len(store.data.SessionMeta))
	copy(out, store.data.SessionMeta)
	return out
}

// UpsertSessionMeta 写入会话标题/置顶/归档偏好。
func (store *PrefsStore) UpsertSessionMeta(meta SessionMeta) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.data.SessionMeta {
		if store.data.SessionMeta[i].SessionID == meta.SessionID {
			store.data.SessionMeta[i] = meta
			return store.save()
		}
	}
	store.data.SessionMeta = append(store.data.SessionMeta, meta)
	return store.save()
}

// defaultShutdownTimeout 是退出时等待运行收尾的上限。
const defaultShutdownTimeout = 8 * time.Second

// RunController 管理异步运行与事件推送。
// 运行上下文绑定应用生命周期，而不是单次 binding 调用寿命；
// 用户取消与退出收尾仍走显式 cancel/Shutdown，不丢失 Journal 合同。
type RunController struct {
	mu       sync.Mutex
	service  app.Service
	emit     Emitter
	prefs    *PrefsStore
	cancel   map[string]context.CancelFunc
	session  map[string]string
	lastSeq  map[string]uint64
	running  map[string]bool
	steering map[string]*agent.SteeringBridge
	baseCtx  context.Context
	baseStop context.CancelFunc
	wg       sync.WaitGroup
	closing  bool
}

// NewRunController 创建控制器；emit 可为 nil。
// 控制器自带应用级上下文，后台运行不再绑定 binding ctx。
