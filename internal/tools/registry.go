// Package tools contains Cairn's fixed tool registry. It deliberately has no
// dynamic discovery or plugin loading surface.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/ports"
)

type Result struct {
	OK        bool            `json:"ok"`
	Code      string          `json:"code"`
	Data      json.RawMessage `json:"data,omitempty"`
	Preview   string          `json:"preview,omitempty"`
	Truncated bool            `json:"truncated"`
	Bytes     int64           `json:"bytes"`
	SHA256    string          `json:"sha256,omitempty"`
}

type Tool interface {
	Spec() ports.ToolSpec
	Execute(ctx context.Context, raw json.RawMessage) Result
}

type SideEffectClass string

const (
	SideEffectNone      SideEffectClass = "none"
	SideEffectWorkspace SideEffectClass = "workspace"
	SideEffectProcess   SideEffectClass = "process"
)

type SideEffectTool interface {
	SideEffect() SideEffectClass
}

type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry(available ...Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]Tool, len(available))}
	for _, tool := range available {
		if tool == nil {
			return nil, fmt.Errorf("tool registry cannot contain nil")
		}
		name := tool.Spec().Name
		if name == "" {
			return nil, fmt.Errorf("tool registry cannot contain unnamed tool")
		}
		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", name)
		}
		registry.tools[name] = tool
		registry.order = append(registry.order, name)
	}
	return registry, nil
}

func (registry *Registry) Specs() []ports.ToolSpec {
	specs := make([]ports.ToolSpec, 0, len(registry.order))
	for _, name := range registry.order {
		specs = append(specs, registry.tools[name].Spec())
	}
	return specs
}

func (registry *Registry) Execute(ctx context.Context, name string, arguments json.RawMessage) Result {
	tool, exists := registry.tools[name]
	if !exists {
		return Failure("unknown_tool", "unknown tool: "+name)
	}
	return tool.Execute(ctx, arguments)
}

func (registry *Registry) SideEffect(name string) SideEffectClass {
	tool, exists := registry.tools[name]
	if !exists {
		return SideEffectNone
	}
	if classified, ok := tool.(SideEffectTool); ok {
		return classified.SideEffect()
	}
	return SideEffectNone
}

func Failure(code, preview string) Result { return Result{Code: code, Preview: preview} }

func Success(data any, preview string, bytes int64, digest string, truncated bool) Result {
	encoded, err := json.Marshal(data)
	if err != nil {
		return Failure("internal", err.Error())
	}
	return Result{OK: true, Code: "ok", Data: encoded, Preview: preview, Bytes: bytes, SHA256: digest, Truncated: truncated}
}
