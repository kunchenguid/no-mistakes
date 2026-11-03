package shellenv

import "testing"

func TestLiveJobProcesses(t *testing.T) {
	t.Parallel()
	const leader = 100
	tests := []struct {
		name  string
		procs []jobProcess
		want  int
	}{
		{name: "empty job", want: 0},
		{name: "stale exited entry for the leader", procs: []jobProcess{{pid: leader, state: jobProcessExited}}, want: 0},
		{name: "console host the leader started", procs: []jobProcess{
			{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true},
		}, want: 0},
		{name: "console host and a stale leader entry", procs: []jobProcess{
			{pid: leader, state: jobProcessExited},
			{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true},
		}, want: 0},
		{name: "running child is refused", procs: []jobProcess{
			{pid: 300, parentPID: leader, state: jobProcessRunning},
		}, want: 1},
		{name: "running child beside the console host", procs: []jobProcess{
			{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true},
			{pid: 300, parentPID: leader, state: jobProcessRunning},
		}, want: 1},
		{name: "console host of a grandchild is not the leader's", procs: []jobProcess{
			{pid: 400, parentPID: 300, state: jobProcessRunning, systemConsoleHost: true},
		}, want: 1},
		{name: "process named like a console host elsewhere is a child", procs: []jobProcess{
			{pid: 500, parentPID: leader, state: jobProcessRunning, systemConsoleHost: false},
		}, want: 1},
		{name: "uninspectable process fails closed", procs: []jobProcess{
			{pid: 600, state: jobProcessUnknown},
		}, want: 1},
		{name: "uninspectable console host candidate fails closed", procs: []jobProcess{
			{pid: 700, parentPID: leader, state: jobProcessUnknown, systemConsoleHost: true},
		}, want: 1},
		{name: "several live children are all counted", procs: []jobProcess{
			{pid: 300, state: jobProcessRunning},
			{pid: 301, state: jobProcessRunning},
			{pid: 302, state: jobProcessExited},
		}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := liveJobProcesses(tt.procs, leader); got != tt.want {
				t.Fatalf("liveJobProcesses() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestJobHasLiveProcesses(t *testing.T) {
	t.Parallel()
	const leader = 100
	stale := []jobProcess{{pid: leader, state: jobProcessExited}}
	tests := []struct {
		name                    string
		totalBefore, totalAfter uint32
		procs                   []jobProcess
		want                    bool
	}{
		{name: "clean exit with a stable total", totalBefore: 1, totalAfter: 1, procs: stale, want: false},
		{name: "console host only with a stable total", totalBefore: 2, totalAfter: 2, procs: []jobProcess{
			{pid: leader, state: jobProcessExited},
			{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true},
		}, want: false},
		{name: "running child with a stable total", totalBefore: 2, totalAfter: 2, procs: []jobProcess{
			{pid: 300, parentPID: leader, state: jobProcessRunning},
		}, want: true},
		// A listed child spawned a process after the list was read and exited
		// before it was inspected: every listed entry reads as exited, only the
		// counter shows the new member.
		{name: "grandchild started after the list was read", totalBefore: 2, totalAfter: 3, procs: []jobProcess{
			{pid: leader, state: jobProcessExited},
			{pid: 300, parentPID: leader, state: jobProcessExited},
		}, want: true},
		{name: "total moved with an empty list", totalBefore: 1, totalAfter: 2, want: true},
		{name: "total moved beside the console host", totalBefore: 2, totalAfter: 3, procs: []jobProcess{
			{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true},
		}, want: true},
		{name: "several processes started while inspecting", totalBefore: 2, totalAfter: 5, procs: stale, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := jobHasLiveProcesses(tt.totalBefore, tt.totalAfter, tt.procs, leader); got != tt.want {
				t.Fatalf("jobHasLiveProcesses(%d, %d) = %v, want %v", tt.totalBefore, tt.totalAfter, got, tt.want)
			}
		})
	}
}
