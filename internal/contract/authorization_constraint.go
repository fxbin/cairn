// [INPUT]: 闭合授权效果词表。
// [OUTPUT]: 提供仅效果与说明的授权收紧候选。
// [POS]: 扩展约束不接触审批指纹、执行绑定或副作用。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package contract 定义共享的纯数据与校验合同。
// Author: fxbin
package contract

// AuthorizationConstraint 只表达可选约束的效果与说明。
// Effect 为空时保持原决定；宿主校验非空效果只会收紧。
// 本合同不携带审批指纹、工作区观察或执行绑定。
type AuthorizationConstraint struct {
	Effect  AuthorizationEffect
	RuleID  string
	Summary string
}
