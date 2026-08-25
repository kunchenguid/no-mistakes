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
			name: "json-escaped windows home prefix",
			in:   `{"path":"C:\\Users\\testuser\\x.log"}`,
			want: `{"path":"~\\x.log"}`,
		},
		{
			name: "json-escaped windows bare home",
			in:   `{"path":"C:\\Users\\testuser"}`,
			want: `{"path":"~"}`,
		},
		{
			name: "json-escaped newline before home path",
			in:   `{"log":"line1\n/home/testuser/x.log"}`,
			want: `{"log":"line1\n~/x.log"}`,
		},
		{
			name: "repeated json-escaped newline separated home paths",
			in:   `first /home/testuser/a\n/home/testuser/b`,
			want: `first ~/a\n~/b`,
		},
		{
			name: "json-escaped newline before windows home",
			in:   `trace\nC:\Users\testuser\x.log`,
			want: `trace\n~\x.log`,
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
			name: "colon-separated path entries",
			in:   "PATH=/home/testuser/bin:/home/testuser/go/bin",
			want: "PATH=~/bin:~/go/bin",
		},
		{
			name: "docker bind target",
			in:   "-v /data:/home/testuser/app",
			want: "-v /data:~/app",
		},
		{
			name: "include-flag-attached path",
			in:   "-I/home/testuser/include",
			want: "-I~/include",
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
			name: "url with users path after scheme",
			in:   "fetch https://users/list first",
		},
		{
			name: "url with home path after scheme",
			in:   "fetch https://home/x first",
		},
		{
			name: "cdn url with data path",
			in:   "fetch https://cdn.example.com/data/file.json first",
		},
		{
			// Deliberate tradeoff: a doubled forward slash before the root is
			// rare, while accepting it would mangle every https:// URL whose
			// path starts with users or home.
			name: "doubled forward slash before home root",
			in:   "//home/testuser/x",
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
			name: "repo-relative path with users segment and line suffix",
			in:   "src/users/service.ts:42",
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
// or redirected home. Only the process's own home directory can catch those,
// and the same is true of an own-home path embedded in an http(s) URL: the
// conventional-root rules deliberately never match inside a URL.
//
// Both HOME and USERPROFILE are set, because os.UserHomeDir consults a
// different one per platform (USERPROFILE on Windows, HOME elsewhere) and this
// property has to hold on all of them.
func TestRedactText_UnconventionalHomeFromEnvironment(t *testing.T) {
	tests := []struct {
		name string
		home string
		in   string
		want string
	}{
		{name: "posix prefix", home: "/srv/nm-operator", in: "/srv/nm-operator/.no-mistakes/evidence/x.log", want: "~/.no-mistakes/evidence/x.log"},
		{name: "posix bare", home: "/srv/nm-operator", in: "HOME=/srv/nm-operator", want: "HOME=~"},
		{name: "posix sibling directory is not the home", home: "/srv/nm-operator", in: "/srv/nm-operator-backup/x", want: "/srv/nm-operator-backup/x"},
		{name: "posix suffix match is not a prefix match", home: "/srv/nm-operator", in: "/opt/srv/nm-operator/x", want: "/opt/srv/nm-operator/x"},
		{name: "posix repeated", home: "/srv/nm-operator", in: "/srv/nm-operator/a and /srv/nm-operator/b", want: "~/a and ~/b"},
		{name: "posix colon-separated path entries", home: "/srv/nm-operator", in: "PATH=/srv/nm-operator/bin:/srv/nm-operator/go/bin", want: "PATH=~/bin:~/go/bin"},
		{name: "posix flag-attached path", home: "/srv/nm-operator", in: "-I/srv/nm-operator/include", want: "-I~/include"},
		// A redirected Windows profile: absolute, but not under C:\Users, so
		// only the process's own home can catch it.
		{name: "windows redirected profile", home: `D:\profiles\operator`, in: `D:\profiles\operator\.no-mistakes\evidence\x.log`, want: `~\.no-mistakes\evidence\x.log`},
		{name: "windows redirected profile forward slashes", home: `D:\profiles\operator`, in: "D:/profiles/operator/evidence/x.log", want: "~/evidence/x.log"},
		{name: "windows redirected profile flag-attached path", home: `D:\profiles\operator`, in: "-LD:/profiles/operator/lib", want: "-L~/lib"},
		{name: "windows redirected profile json-escaped", home: `D:\profiles\operator`, in: `{"path":"D:\\profiles\\operator\\x.log"}`, want: `{"path":"~\\x.log"}`},
		// Git Bash, MSYS2, and Cygwin set a POSIX-rooted HOME on Windows, and
		// captured output from a test run under those shells prints it. The
		// conventional-root rules cannot see it: the "/Users" in
		// "/c/Users/operator" is preceded by a drive letter, which the URL
		// guard treats as an ordinary path segment.
		{name: "msys drive-mapped home", home: "/c/Users/operator", in: "rootdir: /c/Users/operator/.no-mistakes/worktrees/ab12cd/1/svc", want: "rootdir: ~/.no-mistakes/worktrees/ab12cd/1/svc"},
		{name: "msys drive-mapped home flag-attached path", home: "/c/Users/operator", in: "-I/c/Users/operator/include", want: "-I~/include"},
		{name: "cygwin drive-mapped home", home: "/cygdrive/c/Users/operator", in: "rootdir: /cygdrive/c/Users/operator/svc", want: "rootdir: ~/svc"},
		{name: "url-embedded own home", home: "/home/testuser", in: "http://localhost:8543/evidence/home/testuser/run-1/x.log", want: "http://localhost:8543/evidence~/run-1/x.log"},
		{name: "url-embedded own home unconventional root", home: "/srv/nm-operator", in: "https://files.internal/evidence/srv/nm-operator/run-1/x.log", want: "https://files.internal/evidence~/run-1/x.log"},
		{name: "single-segment home after url host is not redacted", home: "/data", in: "https://cdn.example.com/data/file.json", want: "https://cdn.example.com/data/file.json"},
		{name: "single-segment home inside url path is not redacted", home: "/data", in: "https://cdn.example.com/static/data/file.json", want: "https://cdn.example.com/static/data/file.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", tt.home)
			t.Setenv("USERPROFILE", tt.home)
			if got := RedactText(tt.in); got != tt.want {
				t.Fatalf("RedactText(%q) with home %q = %q, want %q", tt.in, tt.home, got, tt.want)
			}
		})
	}
}

// TestUsableHomeCandidate_AcceptsBothPlatformSpellings pins the rule directly,
// because it is the one place a platform-specific helper (filepath.IsAbs)
// would silently disable redaction rather than fail loudly.
func TestUsableHomeCandidate_AcceptsBothPlatformSpellings(t *testing.T) {
	t.Parallel()
	usable := []string{
		"/srv/nm-operator",
		"/home/testuser",
		"/c/Users/operator",
		`C:\Users\testuser`,
		"C:/Users/testuser",
		`D:\profiles\operator`,
		`\\fileserver\profiles\operator`,
	}
	for _, home := range usable {
		if !usableHomeCandidate(home) {
			t.Errorf("usableHomeCandidate(%q) = false, want true", home)
		}
	}
	unusable := []string{
		"",
		"/",
		"//",
		"////",
		`C:\`,
		"C:",
		"relative/path",
		"...",
	}
	for _, home := range unusable {
		if usableHomeCandidate(home) {
			t.Errorf("usableHomeCandidate(%q) = true, want false", home)
		}
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
		`C:\\Users\\testuser\\a`,
		"file:///home/testuser/a",
		`{"log":"line1\n/home/testuser/x"}`,
		strings.Repeat("/home/testuser/x ", 50),
		"no paths here at all",
	} {
		if got := RedactText(in); len(got) > len(in) {
			t.Fatalf("RedactText(%q) grew from %d to %d bytes", in, len(in), len(got))
		}
	}
}

// TestHomeCandidates_AreSeparatorSpellingIndependent pins that candidate
// resolution does not depend on the platform the binary was built for. Without
// it, filepath.Clean would normalise separators one way on Windows and another
// on Linux, and a green Linux test run would say nothing about what a Windows
// daemon actually redacts.
func TestHomeCandidates_AreSeparatorSpellingIndependent(t *testing.T) {
	for _, home := range []string{"/srv/nm-operator", `D:\profiles\operator`} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			got := map[string]bool{}
			for _, candidate := range homeCandidates() {
				got[candidate] = true
			}
			for _, want := range []string{
				strings.ReplaceAll(home, `\`, "/"),
				strings.ReplaceAll(home, "/", `\`),
				strings.ReplaceAll(home, `\`, `\\`),
			} {
				if !got[want] {
					t.Errorf("home %q: candidates %v missing spelling %q", home, got, want)
				}
			}
		})
	}
}
