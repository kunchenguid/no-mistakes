package procreap

import (
	"context"
	"errors"
	"testing"
)

func TestFixProgressFailureQuiescenceRefusesUnreadableProcessTable(t *testing.T) {
	old := listProcessesFunc
	t.Cleanup(func() { listProcessesFunc = old })
	listProcessesFunc = func() ([]Process, error) { return nil, errors.New("process table unreadable") }
	if err := Quiesce(context.Background(), Options{WorktreesRoot: t.TempDir()}); err == nil {
		t.Fatal("unproven shutdown accepted")
	}
}
