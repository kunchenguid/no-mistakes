package procreap

import (
	"testing"
	"time"
)

func TestHasLiveDescendant(t *testing.T) {
	t.Parallel()
	const agent = 100
	cases := []struct {
		name  string
		procs []processState
		want  bool
	}{
		{"no children", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}}, false},
		{"direct child", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 101, PPID: agent, PGID: 7, Stat: "R"}}, true},
		{"grandchild after its parent moved group", []processState{
			{PID: agent, PPID: 1, PGID: agent, Stat: "S"},
			{PID: 101, PPID: agent, PGID: 101, Stat: "S"},
			{PID: 102, PPID: 101, PGID: 101, Stat: "R"},
		}, true},
		{"reparented group member", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 103, PPID: 1, PGID: agent, Stat: "S"}}, true},
		{"zombie child only", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 104, PPID: agent, PGID: agent, Stat: "Z"}}, false},
		{"unrelated process", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 105, PPID: 50, PGID: 50, Stat: "S"}}, false},
		{"helper started before the bound", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 108, PPID: agent, PGID: agent, Stat: "S", Elapsed: time.Hour}}, false},
		{"tool child of a long-lived helper", []processState{
			{PID: agent, PPID: 1, PGID: agent, Stat: "S"},
			{PID: 109, PPID: agent, PGID: 109, Stat: "S", Elapsed: time.Hour},
			{PID: 110, PPID: 109, PGID: 109, Stat: "R", Elapsed: time.Second},
		}, true},
		{"parent cycle", []processState{{PID: 106, PPID: 107, PGID: 9, Stat: "S"}, {PID: 107, PPID: 106, PGID: 9, Stat: "S"}}, false},
	}
	for _, tc := range cases {
		if got := hasLiveDescendant(agent, tc.procs, time.Minute); got != tc.want {
			t.Errorf("%s: hasLiveDescendant = %v, want %v", tc.name, got, tc.want)
		}
	}
}
