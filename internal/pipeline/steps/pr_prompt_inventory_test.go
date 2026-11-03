package steps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestPRPromptInventoryCountsAtByteLimit(t *testing.T) {
	for _, count := range []int{999, 1000, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var raw strings.Builder
			// The oversized anchor cannot fit. Later, short modified anchors
			// must still fit, including when shown/omitted counts change width.
			fmt.Fprintf(&raw, "R100\told.go\tinternal/%s.go\n", strings.Repeat("long", 2048))
			for i := 1; i < count; i++ {
				fmt.Fprintf(&raw, "M\tinternal/f%04d.go\n", i)
			}
			sctx := &pipeline.StepContext{WorkDir: "/fixture", Run: &db.Run{HeadSHA: strings.Repeat("b", 40)}}
			base := strings.Repeat("a", 40)
			output := prPromptInventory(sctx, raw.String(), base)
			if len(output) > maxPRPromptInventoryBytes {
				t.Fatalf("inventory exceeds byte limit: %d", len(output))
			}
			var total, shown, omitted int
			if _, err := fmt.Sscanf(output, "%d inventory lines: %d shown, %d omitted", &total, &shown, &omitted); err != nil {
				t.Fatal(err)
			}
			if total != count || shown+omitted != count || shown < 100 {
				t.Fatalf("incorrect counts: total=%d shown=%d omitted=%d", total, shown, omitted)
			}
			_, sample, ok := strings.Cut(output, "then bounded samples:\n")
			if !ok {
				t.Fatal("sample section missing")
			}
			sample, _, ok = strings.Cut(sample, "\n\nRecover the complete inventory")
			if !ok {
				t.Fatal("recovery section missing")
			}
			entries := strings.Split(sample, "\n")
			if len(entries) != shown || entries[0] != "M\tinternal/f0001.go" {
				t.Fatalf("sample does not match shown count or skipped anchor: %d entries, first=%q", len(entries), entries[0])
			}
			for i, entry := range entries {
				if entry != fmt.Sprintf("M\tinternal/f%04d.go", i+1) {
					t.Fatalf("sample order changed at %d: %q", i, entry)
				}
			}
			if !strings.Contains(output, fmt.Sprintf(`"internal": %d changed, %d shown, %d omitted across 1 directories`, total, shown, omitted)) {
				t.Fatal("group counts differ from sample counts")
			}
			if len(output)+len(fmt.Sprintf("\nM\tinternal/f%04d.go", shown+1)) <= maxPRPromptInventoryBytes {
				t.Fatal("a fitting modified anchor was omitted")
			}
			if repeated := prPromptInventory(sctx, raw.String(), base); repeated != output {
				t.Fatal("inventory output is not deterministic")
			}
		})
	}
}

func BenchmarkPRPromptInventoryManyGroups(b *testing.B) {
	for _, count := range []int{1222, 5000, 10000} {
		b.Run(fmt.Sprintf("groups-%d", count), func(b *testing.B) {
			var raw strings.Builder
			for i := range count {
				fmt.Fprintf(&raw, "A\tgroup-%05d-%s/index.json\n", i, strings.Repeat("long-name-", 4))
			}
			inventory := raw.String()
			sctx := &pipeline.StepContext{WorkDir: "/fixture", Run: &db.Run{HeadSHA: strings.Repeat("b", 40)}}
			base := strings.Repeat("a", 40)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				output := prPromptInventory(sctx, inventory, base)
				if len(output) > maxPRPromptInventoryBytes {
					b.Fatalf("inventory exceeds byte limit: %d", len(output))
				}
			}
		})
	}
}
