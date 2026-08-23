package safepath

import (
	"strings"
	"testing"
)

// Every fixture here uses a synthetic home ("/home/testuser", "/Users/testuser",
// "C:\\Users\\testuser"). Nothing in this package's tests may name a real
// account: a test fixture is published source, exactly like the PR bodies this
// package exists to keep clean.

func TestRedactText_ConventionalHomeRoots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "linux home prefix",
			in:   "/home/testuser/.no-mistakes/evidence/run-1/pytest.log",
			want: "~/.no-mistakes/evidence/run-1/pytest.log",
		},
		{
			name: "macos home prefix",
			in:   "/Users/testuser/.no-mistakes/evidence/run-1/pytest.log",
			want: "~/.no-mistakes/evidence/run-1/pytest.log",
		},
		{
			name: "windows home prefix",
			in:   `C:\Users\testuser\.no-mistakes\evidence\run-1\pytest.log`,
			want: `~\.no-mistakes\evidence\run-1\pytest.log`,
		},
		{
			name: "windows home prefix with forward slashes",
			in:   "C:/Users/testuser/.no-mistakes/evidence/run-1/pytest.log",
			want: "~/.no-mistakes/evidence/run-1/pytest.log",
		},
		{
			name: "bare home directory",
			in:   "cwd is /home/testuser",
			want: "cwd is ~",
		},
		{
			name: "file url",
			in:   "opened file:///home/testuser/repo/index.html",
			want: "opened ~/repo/index.html",
		},
		{
			name: "case-insensitive root",
			in:   "/HOME/testuser/repo",
			want: "~/repo",
		},
		{
			name: "pytest rootdir header",
			in:   "rootdir: /home/testuser/.no-mistakes/worktrees/ab12cd/1/svc",
			want: "rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc",
		},
		{
			name: "quoted worktree assignment",
			in:   `WORKTREE = "/home/testuser/.no-mistakes/worktrees/ab12cd/1/svc"`,
			want: `WORKTREE = "~/.no-mistakes/worktrees/ab12cd/1/svc"`,
		},
		{
			name: "inside an html code span",
			in:   "<code>/home/testuser/evidence/out.log</code>",
			want: "<code>~/evidence/out.log</code>",
		},
		{
			name: "html-escaped newline joiner keeps its entity",
			in:   "/home/testuser/a.log&#10;next line",
			want: "~/a.log&#10;next line",
		},
		{
			name: "flag value",
			in:   "--basetemp=/home/testuser/tmp/pytest-of-testuser",
			want: "--basetemp=~/tmp/pytest-of-testuser",
		},
		{
			name: "every occurrence, not just the first",
			in: "rootdir: /home/testuser/svc\n" +
				"cachedir: /home/testuser/svc/.pytest_cache\n" +
				"configfile: /home/testuser/svc/pyproject.toml\n",
			want: "rootdir: ~/svc\ncachedir: ~/svc/.pytest_cache\nconfigfile: ~/svc/pyproject.toml\n",
		},
		{
			name: "adjacent occurrences separated by one space",
			in:   "/home/testuser/a /home/testuser/b /home/testuser/c",
			want: "~/a ~/b ~/c",
		},
		{
			name: "inside a fenced code block",
			in:   "```text\n= /home/testuser/svc\n```",
			want: "```text\n= ~/svc\n```",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactText(tt.in); got != tt.want {
				t.Fatalf("RedactText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactText_LeavesUnrelatedTextIntact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
	}{
		{
			name: "github api users url",
			in:   "see https://api.github.com/users/octocat for the account",
		},
		{
			name: "project signature url",
			in:   "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)",
		},
		{
			name: "repo-relative path",
			in:   "internal/pipeline/steps/prsummary.go:118",
		},
		{
			name: "system path with no home root",
			in:   "/usr/local/go/bin/go test ./...",
		},
		{
			name: "a directory merely called home",
			in:   "/srv/home/config.yaml",
		},
		{
			name: "prose about a home directory",
			in:   "the home directory is not published",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactText(tt.in); got != tt.in {
				t.Fatalf("RedactText(%q) = %q, want it unchanged", tt.in, got)
			}
		})
	}
}

// TestRedactText_UnconventionalHomeFromEnvironment covers the account whose
// home is not under /home or /Users - a root daemon, a container, a relocated
// home. Only the process's own home directory can catch those.
func TestRedactText_UnconventionalHomeFromEnvironment(t *testing.T) {
	t.Setenv("HOME", "/srv/nm-operator")
	t.Setenv("USERPROFILE", "")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "prefix", in: "/srv/nm-operator/.no-mistakes/evidence/x.log", want: "~/.no-mistakes/evidence/x.log"},
		{name: "bare", in: "HOME=/srv/nm-operator", want: "HOME=~"},
		{name: "sibling directory is not the home", in: "/srv/nm-operator-backup/x", want: "/srv/nm-operator-backup/x"},
		{name: "suffix match is not a prefix match", in: "/opt/srv/nm-operator/x", want: "/opt/srv/nm-operator/x"},
		{name: "repeated", in: "/srv/nm-operator/a and /srv/nm-operator/b", want: "~/a and ~/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactText(tt.in); got != tt.want {
				t.Fatalf("RedactText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRedactText_DegenerateHomeIsIgnored guards the over-redaction limit: a
// home value of "/" would otherwise rewrite every path in the document.
func TestRedactText_DegenerateHomeIsIgnored(t *testing.T) {
	t.Setenv("HOME", "/")
	t.Setenv("USERPROFILE", "")

	const in = "/usr/local/bin/no-mistakes and /etc/hosts"
	if got := RedactText(in); got != in {
		t.Fatalf("RedactText(%q) = %q, want it unchanged", in, got)
	}
}

// TestRedactText_NeverGrowsTheText is what lets the PR-body assembly redact
// after it has already clamped the body to the host's character cap.
func TestRedactText_NeverGrowsTheText(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"/home/testuser/a",
		"/Users/testuser",
		`C:\Users\testuser\a\b`,
		"file:///home/testuser/a",
		strings.Repeat("/home/testuser/x ", 50),
		"no paths here at all",
	} {
		if got := RedactText(in); len(got) > len(in) {
			t.Fatalf("RedactText(%q) grew from %d to %d bytes", in, len(in), len(got))
		}
	}
}
