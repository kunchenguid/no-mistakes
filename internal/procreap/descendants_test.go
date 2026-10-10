package procreap

import (
	"reflect"
	"testing"
)

func TestLiveDescendants(t *testing.T) {
	t.Parallel()
	const agent = 100
	cases := []struct {
		name  string
		procs []processState
		want  map[int]bool
	}{
		{"no children", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}}, map[int]bool{}},
		{"direct child", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 101, PPID: agent, PGID: 7, Stat: "R"}}, map[int]bool{101: true}},
		{"grandchild after its parent moved group", []processState{
			{PID: agent, PPID: 1, PGID: agent, Stat: "S"},
			{PID: 101, PPID: agent, PGID: 101, Stat: "S"},
			{PID: 102, PPID: 101, PGID: 101, Stat: "R"},
		}, map[int]bool{101: true, 102: true}},
		{"reparented group member", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 103, PPID: 1, PGID: agent, Stat: "S"}}, map[int]bool{103: true}},
		{"zombie child", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 104, PPID: agent, PGID: agent, Stat: "Z"}}, map[int]bool{}},
		{"unrelated process", []processState{{PID: agent, PPID: 1, PGID: agent, Stat: "S"}, {PID: 105, PPID: 50, PGID: 50, Stat: "S"}}, map[int]bool{}},
		{"parent cycle", []processState{{PID: 106, PPID: 107, PGID: 9, Stat: "S"}, {PID: 107, PPID: 106, PGID: 9, Stat: "S"}}, map[int]bool{}},
	}
	for _, tc := range cases {
		if got := liveDescendants(agent, tc.procs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: liveDescendants = %v, want %v", tc.name, got, tc.want)
		}
	}
}
