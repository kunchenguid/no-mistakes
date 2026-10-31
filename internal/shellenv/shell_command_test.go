package shellenv

import (
	"strings"
	"testing"
)

func TestFormatGroupMembers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		members []groupMember
		want    string
	}{
		{name: "no members", want: ""},
		{
			name:    "pid and command",
			members: []groupMember{{pid: 39456, command: "sleep 1.2"}},
			want:    "pid 39456 (sleep 1.2)",
		},
		{
			name:    "missing command degrades to the pid",
			members: []groupMember{{pid: 66195}},
			want:    "pid 66195",
		},
		{
			name: "several members join",
			members: []groupMember{
				{pid: 39456, command: "sleep 1.2"},
				{pid: 66195, command: `C:\agent\child.exe`},
			},
			want: `pid 39456 (sleep 1.2), pid 66195 (C:\agent\child.exe)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatGroupMembers(tt.members); got != tt.want {
				t.Fatalf("formatGroupMembers() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatGroupMembersBoundsTheList(t *testing.T) {
	t.Parallel()
	var members []groupMember
	for i := 0; i < groupMemberReportLimit+3; i++ {
		members = append(members, groupMember{pid: 1000 + i, command: "worker"})
	}
	got := formatGroupMembers(members)
	if !strings.HasSuffix(got, "and 3 more") {
		t.Fatalf("formatGroupMembers() = %q, want the list bounded with a remainder", got)
	}
	if strings.Contains(got, "pid 1004") {
		t.Fatalf("formatGroupMembers() = %q, want only %d members named", got, groupMemberReportLimit)
	}
}

func TestFormatGroupMembersBoundsCommandLength(t *testing.T) {
	t.Parallel()
	got := formatGroupMembers([]groupMember{{pid: 7, command: strings.Repeat("x", groupMemberCommandLimit+50)}})
	if len(got) > groupMemberCommandLimit+20 {
		t.Fatalf("formatGroupMembers() = %d chars, want the command truncated", len(got))
	}
	if !strings.HasSuffix(got, "...)") {
		t.Fatalf("formatGroupMembers() = %q, want an ellipsis on the truncated command", got)
	}
}

func TestOffendingJobProcesses(t *testing.T) {
	t.Parallel()
	const leader = 100
	procs := []jobProcess{
		{pid: leader, state: jobProcessExited},
		{pid: 200, parentPID: leader, state: jobProcessRunning, systemConsoleHost: true, imagePath: `C:\Windows\System32\conhost.exe`},
		{pid: 300, parentPID: leader, state: jobProcessRunning, imagePath: `C:\agent\child.exe`},
		{pid: 400, state: jobProcessUnknown},
	}
	got := offendingJobProcesses(procs, leader)
	if len(got) != 2 || got[0].pid != 300 || got[1].pid != 400 {
		t.Fatalf("offendingJobProcesses() = %+v, want the running child and the uninspectable one", got)
	}
	if got[0].imagePath != `C:\agent\child.exe` {
		t.Fatalf("offendingJobProcesses() dropped the image path: %+v", got[0])
	}
}
