// [INPUT]: 依赖本能力账本、既有冻结提案与内存快照。
// [OUTPUT]: 回归所有账本出口及纯投影的可变引用隔离。
// [POS]: 锁定只读历史合同，不新增状态或运行时消费方。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package decision

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestLedgerReturnedSnapshotsCannotRewriteRecords 锁定账本所有出口的独立所有权。
// 修改返回的证据或复核时刻不得绕过校验、锁与审计改写内部记录。
func TestLedgerReturnedSnapshotsCannotRewriteRecords(t *testing.T) {
	for _, operation := range []string{"propose", "propose_again", "confirm", "review", "revoke", "reject", "get", "query"} {
		t.Run(operation, func(t *testing.T) {
			ledger := NewLedger()
			proposal := fixtureProposal()
			record, err := ledger.Propose(proposal)
			if err != nil {
				t.Fatal(err)
			}
			if operation != "propose" && operation != "propose_again" && operation != "reject" {
				record, err = ledger.Confirm(record.ID, fixtureApproval())
				if err != nil {
					t.Fatal(err)
				}
			}
			switch operation {
			case "propose_again":
				record, err = ledger.Propose(proposal)
			case "review":
				approval := fixtureApproval()
				approval.At = approval.At.Add(time.Hour)
				record, err = ledger.Review(record.ID, approval, DefaultTTL)
			case "revoke":
				record, err = ledger.Revoke(record.ID, RevokeReasonHumanRequest, proposal.Owner, fixtureApproval().At.Add(time.Hour))
			case "reject":
				record, err = ledger.Reject(record.ID, RejectReasonNotApproved, proposal.Owner, fixtureApproval().At)
			case "get":
				record, err = ledger.Get(record.ID)
			case "query":
				var records []DecisionRecord
				records, err = ledger.Query(Query{Project: proposal.Project, Principal: proposal.Owner, Now: fixtureApproval().At.Add(time.Hour)})
				if err == nil {
					if len(records) != 1 {
						t.Fatalf("snapshot count = %d, want 1", len(records))
					}
					record = records[0]
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := ledger.Get(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			auditCount := len(ledger.Audits(record.ID))
			record.Evidence[0].EventHash = strings.Repeat("0", 64)
			if record.ReviewedAt != nil {
				*record.ReviewedAt = time.Time{}
			}
			after, err := ledger.Get(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(after)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != string(frozen) {
				t.Fatal("returned snapshot rewrote an internal record")
			}
			if err := after.Validate(); err != nil {
				t.Fatalf("internal record became invalid: %v", err)
			}
			if len(ledger.Audits(record.ID)) != auditCount {
				t.Fatal("snapshot mutation changed audit history")
			}
		})
	}
}

// TestProjectContextReturnsIndependentReferences 锁定纯投影也不暴露调用方的可变引用。
func TestProjectContextReturnsIndependentReferences(t *testing.T) {
	ledger := NewLedger()
	record, err := ledger.Propose(fixtureProposal())
	if err != nil {
		t.Fatal(err)
	}
	record, err = ledger.Confirm(record.ID, fixtureApproval())
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ProjectContext(record.Project, record.Owner, []DecisionRecord{record}, MaxContextTokenBudget, fixtureApproval().At.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Entries) != 1 || projection.Entries[0].ReviewedAt == nil {
		t.Fatal("confirmed record must produce one reviewed entry")
	}
	projection.Entries[0].Evidence[0].EventHash = strings.Repeat("0", 64)
	*projection.Entries[0].ReviewedAt = time.Time{}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(frozen) {
		t.Fatal("context projection rewrote its input record")
	}
}
