package quotarouting

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestQuotaRecordOrderingPreservesSpellingsAcrossACPAliasChanges(t *testing.T) {
	for _, tc := range []struct{ stored, resolved string }{
		{"cursor", "acp:cursor"}, {"acp:cursor", "cursor"},
		{"devin", "acp:devin"}, {"acp:devin", "devin"},
	} {
		payload := fmt.Sprintf(`{"version":1,"enabled":true,"configured":["claude",%q],"order":[%q,"claude"],"choice":%q}`, tc.stored, tc.stored, tc.stored)
		record, err := ParseRecord(payload)
		if err != nil {
			t.Fatal(err)
		}
		resolved := []types.AgentName{types.AgentClaude, types.AgentName(tc.resolved)}
		got := record.OrderedAgents(resolved)
		want := []types.AgentName{types.AgentName(tc.resolved), types.AgentClaude}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stored=%s resolved=%s order=%v want=%v", tc.stored, tc.resolved, got, want)
		}
		if !reflect.DeepEqual(resolved, []types.AgentName{types.AgentClaude, types.AgentName(tc.resolved)}) {
			t.Fatal("recovery mutated the configured list")
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var persisted Record
		if err := json.Unmarshal(encoded, &persisted); err != nil {
			t.Fatal(err)
		}
		if persisted.Order[0] != tc.stored || persisted.Configured[1] != tc.stored || persisted.CurrentAgent() != tc.stored {
			t.Fatalf("stored spellings changed: %+v", persisted)
		}
	}
}

func TestQuotaEvidenceLookupAcceptsEquivalentACPNames(t *testing.T) {
	for _, tc := range []struct{ configured, query types.AgentName }{
		{"cursor", "acp:cursor"}, {"acp:cursor", "cursor"},
		{"devin", "acp:devin"}, {"acp:devin", "devin"},
	} {
		snapshot := fixtureSnapshot(t, 5, `{"provider":"cursor","quotaSemantics":{"status":"known","effectiveAvailability":[`+knownRow("all_models", 0, exhaustedNow(), 0)+`]}}`)
		selected := Select(snapshot, []Candidate{{Agent: tc.configured, Provider: "cursor"}}, defaultExpectedRunLength)
		evidence := selected.EvidenceFor(tc.query)
		if evidence == nil || evidence.Agent != tc.configured || evidence.Eligible {
			t.Fatalf("configured=%s query=%s evidence=%+v", tc.configured, tc.query, evidence)
		}
	}
}
