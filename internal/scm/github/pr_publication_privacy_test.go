package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/safecontent"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// These tests exercise Host.CreatePR and Host.UpdatePR rather than only the
// scrubber. The injected command factory is the fake network client: its stdin
// assertion proves the raw body was transformed before gh could receive it.
func TestCreatePRScrubsEachSensitiveLocalDataClassBeforeGitHub(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "019ff2f3-5f31-744b-90b8-679074ff7686")
	t.Setenv("HERDR_PANE_ID", "pi:47")

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "user home path",
			raw:  "evidence: /Users/testuser/private/result.log",
			want: "evidence: ~/private/result.log",
		},
		{
			name: "treehouse managed copy",
			raw:  "cwd: /Users/testuser/.treehouse/private-service-a1b2c3/4/private-service/internal/api.go",
			want: "cwd: [worktree]/internal/api.go",
		},
		{
			name: "macos environment temp path",
			raw:  "log: /var/folders/zz/synthetic-cache/T/pi-bash-a1b2c3d4e5f60718.log",
			want: "log: [temp]",
		},
		{
			name: "mktemp path",
			raw:  "log: /tmp/tmp.A1b2C3d4E5/result.log",
			want: "log: [temp]",
		},
		{
			name: "pi session identifier",
			raw:  "pi session: 019ff2f3-5f31-744b-90b8-679074ff7686",
			want: "pi session: [id]",
		},
		{
			name: "herdr session identifier",
			raw:  "HERDR_PANE_ID=pi:47",
			want: "HERDR_PANE_ID=[id]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const command = "gh pr create --head feature/privacy --base main --repo test/repo --title fix: protect publication --body-file -"
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				command: {stdout: "https://github.com/test/repo/pull/42\n", wantStdin: tc.want},
			}), nil, "", "test/repo")

			if _, err := host.CreatePR(context.Background(), "feature/privacy", "main", scm.PRContent{
				Title: "fix: protect publication",
				Body:  tc.raw,
			}); err != nil {
				t.Fatalf("CreatePR() error = %v", err)
			}
		})
	}
}

func TestCreateAndUpdatePRShareFinalPublicationPrivacyBoundary(t *testing.T) {
	t.Setenv("PI_SESSION_ID", "019ff2f3-5f31-744b-90b8-679074ff7686")
	t.Setenv("HERDR_TAB_ID", "pi:52")

	const rawTitle = "fix: remove /Users/testuser/.treehouse/private-service-a1b2c3/4/private-service leak"
	const safeTitle = "fix: remove [worktree] leak"
	rawBody := strings.Join([]string{
		"## Testing",
		"",
		"cwd: /Users/testuser/.treehouse/private-service-a1b2c3/4/private-service/internal/api.go",
		"log one: /tmp/tmp.A1b2C3d4E5/result.log",
		"log two: /var/folders/zz/synthetic-cache/T/pi-bash-a1b2c3d4e5f60718.log",
		"PI_SESSION_ID=019ff2f3-5f31-744b-90b8-679074ff7686",
		"HERDR_TAB_ID=pi:52",
		"again: /Users/testuser/.treehouse/private-service-a1b2c3/4/private-service/pkg/two.go",
	}, "\n")
	safeBody := strings.Join([]string{
		"## Testing",
		"",
		"cwd: [worktree]/internal/api.go",
		"log one: [temp]",
		"log two: [temp]",
		"PI_SESSION_ID=[id]",
		"HERDR_TAB_ID=[id]",
		"again: [worktree]/pkg/two.go",
	}, "\n")

	operations := []struct {
		name    string
		command string
		invoke  func(*Host) error
	}{
		{
			name:    "initial create",
			command: "gh pr create --head feature/privacy --base main --repo test/repo --title " + safeTitle + " --body-file -",
			invoke: func(host *Host) error {
				_, err := host.CreatePR(context.Background(), "feature/privacy", "main", scm.PRContent{Title: rawTitle, Body: rawBody})
				return err
			},
		},
		{
			name:    "later full body update",
			command: "gh pr edit 42 --repo test/repo --title " + safeTitle + " --body-file -",
			invoke: func(host *Host) error {
				_, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: rawTitle, Body: rawBody})
				return err
			},
		},
		{
			name:    "attestation body only update",
			command: "gh pr edit 42 --repo test/repo --body-file -",
			invoke: func(host *Host) error {
				_, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Body: rawBody})
				return err
			},
		},
	}

	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			response := githubTestResponse{wantStdin: safeBody}
			if op.name == "initial create" {
				response.stdout = "https://github.com/test/repo/pull/42\n"
			}
			host := New(githubTestCmdFactory(map[string]githubTestResponse{op.command: response}), nil, "", "test/repo")
			if err := op.invoke(host); err != nil {
				t.Fatalf("publication operation error = %v", err)
			}
		})
	}
}

func TestCreateAndUpdatePRPreserveSafeBodiesByteForByte(t *testing.T) {
	t.Parallel()
	const title = "fix(scm): preserve safe markdown"
	const body = "## Testing\n\n- kept `/tmp/example`\n- see https://example.com/docs\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"0123456789abcdef0123456789abcdef01234567\",\"steps\":[]} -->\n"

	operations := []struct {
		name    string
		command string
		invoke  func(*Host) error
	}{
		{
			name:    "create",
			command: "gh pr create --head feature/safe --base main --repo test/repo --title " + title + " --body-file -",
			invoke: func(host *Host) error {
				_, err := host.CreatePR(context.Background(), "feature/safe", "main", scm.PRContent{Title: title, Body: body})
				return err
			},
		},
		{
			name:    "update",
			command: "gh pr edit 42 --repo test/repo --title " + title + " --body-file -",
			invoke: func(host *Host) error {
				_, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: title, Body: body})
				return err
			},
		},
	}

	for _, op := range operations {
		op := op
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			response := githubTestResponse{wantStdin: body}
			if op.name == "create" {
				response.stdout = "https://github.com/test/repo/pull/42\n"
			}
			host := New(githubTestCmdFactory(map[string]githubTestResponse{op.command: response}), nil, "", "test/repo")
			if err := op.invoke(host); err != nil {
				t.Fatalf("publication operation error = %v", err)
			}
		})
	}
}

func TestCreateAndUpdatePRRefuseUntransformableContentBeforeGitHub(t *testing.T) {
	t.Parallel()
	const raw = "evidence at ~/.treehouse/private-managed-copy/not-a-slot"
	operations := []struct {
		name   string
		invoke func(*Host) error
	}{
		{
			name: "create",
			invoke: func(host *Host) error {
				_, err := host.CreatePR(context.Background(), "feature/privacy", "main", scm.PRContent{Title: "fix: privacy", Body: raw})
				return err
			},
		},
		{
			name: "update",
			invoke: func(host *Host) error {
				_, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Title: "fix: privacy", Body: raw})
				return err
			},
		},
	}

	for _, op := range operations {
		op := op
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			host := New(failIfInvokedCmdFactory(t), nil, "", "test/repo")
			err := op.invoke(host)
			if !errors.Is(err, safecontent.ErrUnsafePullRequestContent) {
				t.Fatalf("publication error = %v, want privacy refusal", err)
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-managed-copy") {
				t.Fatalf("privacy refusal echoed rejected content: %v", err)
			}
		})
	}
}
