package decision

import (
	"strings"
	"testing"
	"time"
)

const (
	// fixtureProject 是测试使用的项目作用域标识。
	fixtureProject = "proj_cairn"
	// fixtureOwner 是测试使用的提出者标识。
	fixtureOwner = "user_owner"
	// fixtureApprover 是测试使用的确认人标识。
	fixtureApprover = "user_approver"
	// fixtureEventHash 是测试使用的 Journal 事件内容哈希。
	fixtureEventHash = "3f786850e387550fdab836ed7e6dc881de23001b3f786850e387550fdab836ed"
	// fixtureArtifactDigest 是测试使用的内容寻址制品摘要。
	fixtureArtifactDigest = "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
	// fixtureCommitID 是测试使用的 Git 提交标识。
	fixtureCommitID = "e83c5163316f89bfbde7d9ab23ca2e25604af290"
)

// fixtureTime 是测试使用的冻结时钟基准。
var fixtureTime = time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)

// fixtureJournalEvidence 返回一条结构合法的 Journal 事件证据定位。
func fixtureJournalEvidence() EvidenceRef {
	return EvidenceRef{
		Kind:      EvidenceKindJournalEvent,
		RunID:     "run_0f1e2d3c",
		EventSeq:  7,
		EventHash: fixtureEventHash,
	}
}

// fixtureProposal 返回一条默认的候选录入输入。
func fixtureProposal() Proposal {
	return Proposal{
		Project:   fixtureProject,
		Owner:     fixtureOwner,
		Kind:      KindAuthoritative,
		Context:   "记忆晋升与撤销语义",
		Decision:  "晋升必须经人工确认并保留来源",
		Rationale: "未经确认的晋升会把候选变成默认行为",
		Evidence:  []EvidenceRef{fixtureJournalEvidence()},
		Now:       fixtureTime,
	}
}

// TestNewProposalRejectsPrivatePathAndCredentialInBody 锁定正文边界：私有
// 路径与凭据式内容都不得进入决策正文，入库前即失败关闭。
func TestNewProposalRejectsPrivatePathAndCredentialInBody(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
	}{
		{name: "绝对家目录路径", field: "decision", value: "配置文件位于 /Users/dev/private/app.yaml"},
		{name: "家目录引用", field: "decision", value: "凭据来自 ~/.ssh/id_rsa"},
		{name: "盘符路径", field: "rationale", value: "参考 C:\\Users\\dev\\notes.md"},
		{name: "文件链接", field: "context", value: "见 file:///Users/dev/secret/plan.txt"},
		{name: "路径跳转", field: "decision", value: "读取 ../../etc/passwd 作为样例"},
		{name: "密钥赋值", field: "decision", value: "接入时写入 api_key=sk-abcdefghijklmnopqrst"},
		{name: "令牌赋值", field: "rationale", value: "保存 token=abcdefghijklmnopqrstuvwx"},
		{name: "Bearer 凭据", field: "decision", value: "调用时带上 Bearer abcdefghijklmnop"},
		{name: "云厂商访问密钥", field: "decision", value: "AKIAIOSFODNN7EXAMPLE"},
		{name: "代码托管令牌", field: "decision", value: "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{name: "裸令牌串", field: "rationale", value: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{name: "凭据词干后的裸值", field: "decision", value: "密码是 password hunter2"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			proposal := fixtureProposal()
			switch testCase.field {
			case "context":
				proposal.Context = testCase.value
			case "decision":
				proposal.Decision = testCase.value
			case "rationale":
				proposal.Rationale = testCase.value
			}
			if _, err := NewProposal(proposal); err == nil {
				t.Fatalf("expected %s with %s to be rejected", testCase.field, testCase.value)
			}
		})
	}
}

// TestNewProposalAcceptsBoundedRelativeContext 锁定正文边界的另一侧：不含
// 绝对路径与凭据的正常表述必须被接受，避免守卫误伤造成决策无法入库。
func TestNewProposalAcceptsBoundedRelativeContext(t *testing.T) {
	proposal := fixtureProposal()
	proposal.Context = "可信记忆的晋升与撤销语义"
	proposal.Decision = "证据来源限定为 journal_event、artifact、git_commit 与 test_result 四类"
	proposal.Rationale = "候选与现行分离，避免未复核结论被当作项目规则引用"
	record, err := NewProposal(proposal)
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if record.Status != StatusProposed {
		t.Fatalf("expected proposed status, got %s", record.Status)
	}
	if record.ID == "" {
		t.Fatal("expected a derived record identifier")
	}
}

// TestNewProposalAcceptsCredentialWordsWithoutSecretValues 锁定凭据守卫的
// 误伤边界：出现凭据词干但没有凭据形态值的正常表述必须被接受，否则账本
// 无法记录与令牌、密钥、密码有关的真实决策。
func TestNewProposalAcceptsCredentialWordsWithoutSecretValues(t *testing.T) {
	cases := []string{
		"token 过期后不再冒充现行事实",
		"secret 轮换周期按项目约定执行",
		"password 策略由安全负责人另行约定",
		"决定：证据来源限定为 journal_event 与 artifact 两类",
	}
	for _, value := range cases {
		proposal := fixtureProposal()
		proposal.Decision = value
		if _, err := NewProposal(proposal); err != nil {
			t.Fatalf("expected %q to be accepted, got %v", value, err)
		}
	}
}

// TestNewProposalRejectsBareSecretMaterial 锁定裸密钥守卫：不带键名的固定
// 格式凭据与凭据词干后的裸值都必须被显式拒绝，否则它们会随正文进入注入
// 上下文。
func TestNewProposalRejectsBareSecretMaterial(t *testing.T) {
	cases := []string{
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_abcdefghijklmnopqrstuvwxyz012345",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"password hunter2",
	}
	for _, value := range cases {
		proposal := fixtureProposal()
		proposal.Decision = value
		if _, err := NewProposal(proposal); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

// TestNewProposalRequiresEvidence 锁定证据下限：没有可核查证据定位的结论
// 不能成为候选记录。
func TestNewProposalRequiresEvidence(t *testing.T) {
	proposal := fixtureProposal()
	proposal.Evidence = nil
	if _, err := NewProposal(proposal); err == nil {
		t.Fatal("expected a proposal without evidence to be rejected")
	}
}

// TestNewProposalRejectsOversizedBody 锁定正文长度上限，防止账本退化为自由
// 文本仓库。
func TestNewProposalRejectsOversizedBody(t *testing.T) {
	proposal := fixtureProposal()
	proposal.Decision = strings.Repeat("决", MaxDecisionRunes+1)
	if _, err := NewProposal(proposal); err == nil {
		t.Fatal("expected an oversized decision statement to be rejected")
	}
}

// TestValidTransitionStateMachine 锁定状态迁移：候选只能被确认、拒绝、撤销
// 或到期，现行决策只能被替代、撤销或到期，终态不可再迁出。
func TestValidTransitionStateMachine(t *testing.T) {
	allowed := [][2]string{
		{StatusProposed, StatusActive},
		{StatusProposed, StatusRejected},
		{StatusProposed, StatusRevoked},
		{StatusProposed, StatusExpired},
		{StatusActive, StatusSuperseded},
		{StatusActive, StatusRevoked},
		{StatusActive, StatusExpired},
	}
	for _, transition := range allowed {
		if err := ValidTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("expected transition %s -> %s to be permitted: %v", transition[0], transition[1], err)
		}
	}
	forbidden := [][2]string{
		{StatusProposed, StatusSuperseded},
		{StatusActive, StatusActive},
		{StatusRevoked, StatusActive},
		{StatusRejected, StatusActive},
		{StatusExpired, StatusActive},
		{StatusSuperseded, StatusActive},
	}
	for _, transition := range forbidden {
		if err := ValidTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("expected transition %s -> %s to be rejected", transition[0], transition[1])
		}
	}
}

// TestEffectiveStatusExpiresRecord 锁定过期语义：保留期耗尽后记录的有效
// 状态变为过期，不再冒充现行事实。
func TestEffectiveStatusExpiresRecord(t *testing.T) {
	record, err := NewProposal(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status := record.EffectiveStatus(record.ExpiresAt.Add(-time.Second)); status != StatusProposed {
		t.Fatalf("expected the record to stay proposed before expiry, got %s", status)
	}
	if status := record.EffectiveStatus(record.ExpiresAt); status != StatusExpired {
		t.Fatalf("expected the record to expire at the retention deadline, got %s", status)
	}
	if record.IsAuthoritative(record.ExpiresAt) {
		t.Fatal("an expired record must never be authoritative")
	}
}

// TestVisibleToScopesProjectAndOwner 锁定作用域：跨项目记录一律不可见，
// 个人偏好只对记录所有者可见。
func TestVisibleToScopesProjectAndOwner(t *testing.T) {
	record, err := NewProposal(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if record.VisibleTo("proj_other", fixtureOwner) {
		t.Fatal("a record must never be visible to another project")
	}
	if !record.VisibleTo(fixtureProject, fixtureOwner) {
		t.Fatal("an authoritative record must be visible inside its project")
	}
	preference := fixtureProposal()
	preference.Kind = KindPreference
	preference.Owner = "user_private"
	owned, err := NewProposal(preference)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owned.VisibleTo(fixtureProject, "user_other") {
		t.Fatal("a personal preference must never be visible to another principal")
	}
	if !owned.VisibleTo(fixtureProject, "user_private") {
		t.Fatal("a personal preference must stay visible to its owner")
	}
}

// TestEvidenceRefRejectsForeignSourceFields 锁定证据字段归属：一条证据不能
// 同时冒充多个来源，避免核查时被当作更强的证据。
func TestEvidenceRefRejectsForeignSourceFields(t *testing.T) {
	mixed := fixtureJournalEvidence()
	mixed.Digest = fixtureArtifactDigest
	if err := mixed.Validate(); err == nil {
		t.Fatal("expected a journal reference carrying an artifact digest to be rejected")
	}
	artifactRef := EvidenceRef{Kind: EvidenceKindArtifact, Digest: fixtureArtifactDigest}
	if err := artifactRef.Validate(); err != nil {
		t.Fatalf("expected a bare artifact reference to be accepted: %v", err)
	}
	commitRef := EvidenceRef{Kind: EvidenceKindGitCommit, CommitID: fixtureCommitID}
	if err := commitRef.Validate(); err != nil {
		t.Fatalf("expected a bare commit reference to be accepted: %v", err)
	}
	unknown := EvidenceRef{Kind: "wiki_page"}
	if err := unknown.Validate(); err == nil {
		t.Fatal("expected an unsupported evidence kind to be rejected")
	}
}

// TestRecordIDIsScopeAndContentBound 锁定标识派生：同一提案在同一时刻恒定
// 命中同一标识，作用域或创建时刻不同则标识不同。
func TestRecordIDIsScopeAndContentBound(t *testing.T) {
	first, err := NewProposal(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := NewProposal(fixtureProposal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected a stable identifier, got %s and %s", first.ID, second.ID)
	}
	later := fixtureProposal()
	later.Now = fixtureTime.Add(time.Second)
	third, err := NewProposal(later)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.ID == third.ID {
		t.Fatal("expected a different creation moment to yield a different identifier")
	}
	other := fixtureProposal()
	other.Project = "proj_other"
	fourth, err := NewProposal(other)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.ID == fourth.ID {
		t.Fatal("expected a different project scope to yield a different identifier")
	}
}

// TestContentKeyIdentifiesEquivalentDecisions 锁定等价判定：内容相同的
// 提案共享内容指纹，与创建时刻无关。
func TestContentKeyIdentifiesEquivalentDecisions(t *testing.T) {
	proposal := fixtureProposal()
	first, err := ContentKey(proposal.Project, proposal.Owner, proposal.Kind, proposal.Context, proposal.Decision, proposal.Rationale, proposal.Evidence)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	proposal.Now = fixtureTime.Add(72 * time.Hour)
	second, err := ContentKey(proposal.Project, proposal.Owner, proposal.Kind, proposal.Context, proposal.Decision, proposal.Rationale, proposal.Evidence)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Fatal("expected equivalent decisions to share one content key")
	}
	proposal.Decision = "晋升必须经人工确认并保留来源与作废链"
	third, err := ContentKey(proposal.Project, proposal.Owner, proposal.Kind, proposal.Context, proposal.Decision, proposal.Rationale, proposal.Evidence)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == third {
		t.Fatal("expected a changed statement to yield a different content key")
	}
}
