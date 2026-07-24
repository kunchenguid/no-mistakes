package scm

import (
	"context"
	"errors"
	"testing"
)

func TestExtractHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"https with .git", "https://github.com/user/repo.git", "github.com"},
		{"scp ssh", "git@github.com:user/repo.git", "github.com"},
		{"https self-hosted", "https://gitlab.example.com/group/repo.git", "gitlab.example.com"},
		{"scp ssh nested path", "git@gitlab.example.com:group/sub/repo.git", "gitlab.example.com"},
		{"ssh url with port", "ssh://git@code.example.com:2222/group/repo.git", "code.example.com"},
		{"https userinfo and port", "https://user:token@code.example.com:8443/group/repo.git", "code.example.com"},
		{"git protocol", "git://code.example.com/group/repo.git", "code.example.com"},
		{"mixed case lowercased", "https://CODE.Example.COM/group/repo", "code.example.com"},
		{"ipv6 literal with port", "ssh://git@[::1]:22/group/repo.git", "[::1]"},
		// A '@' inside the path must not be mistaken for a "user@" userinfo
		// prefix: host extraction has to split off the path first.
		{"at-sign in path https", "https://code.example.com/group@prod/repo.git", "code.example.com"},
		{"at-sign in path scp", "git@code.example.com:group@prod/repo.git", "code.example.com"},
		{"at-sign in path with userinfo", "https://user:token@code.example.com/group@prod/repo.git", "code.example.com"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractHost(tt.remote); got != tt.want {
				t.Errorf("ExtractHost(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		})
	}
}

func TestPRSelectorPrefersNumberAndFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pr   *PR
		want string
		ok   bool
	}{
		{name: "number", pr: &PR{Number: " 123 ", URL: "https://example.test/pull/456"}, want: "123", ok: true},
		{name: "URL", pr: &PR{URL: " https://example.test/pull/123 "}, want: "https://example.test/pull/123", ok: true},
		{name: "empty", pr: &PR{}, ok: false},
		{name: "nil", pr: nil, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PRSelector(tt.pr)
			if tt.ok {
				if err != nil {
					t.Fatalf("PRSelector() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("PRSelector() = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("PRSelector() = %q, want error", got)
			}
		})
	}
}

func TestPRNumberUsesURLWhenNumberIsAbsent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pr   *PR
		want string
		ok   bool
	}{
		{name: "number", pr: &PR{Number: " 123 "}, want: "123", ok: true},
		{name: "URL", pr: &PR{URL: "https://gitlab.example.test/group/project/-/merge_requests/42"}, want: "42", ok: true},
		{name: "invalid number falls back to URL", pr: &PR{Number: "latest", URL: "https://gitlab.example.test/group/project/-/merge_requests/42"}, want: "42", ok: true},
		{name: "invalid URL", pr: &PR{URL: "https://example.test/pull/latest"}, ok: false},
		{name: "negative number", pr: &PR{Number: "-1"}, ok: false},
		{name: "zero number", pr: &PR{Number: "0"}, ok: false},
		{name: "number suffix", pr: &PR{Number: "12x"}, ok: false},
		{name: "option-like number", pr: &PR{Number: "--help"}, ok: false},
		{name: "empty", pr: &PR{}, ok: false},
		{name: "nil", pr: nil, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PRNumber(tt.pr)
			if tt.ok {
				if err != nil {
					t.Fatalf("PRNumber() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("PRNumber() = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("PRNumber() = %q, want error", got)
			}
		})
	}
}

func TestCheckBucketHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		check       Check
		wantFailing bool
		wantPending bool
	}{
		{"pass", Check{Bucket: CheckBucketPass}, false, false},
		{"fail", Check{Bucket: CheckBucketFail}, true, false},
		{"pending", Check{Bucket: CheckBucketPending}, false, true},
		{"cancel", Check{Bucket: CheckBucketCancel}, false, false},
		{"skip", Check{Bucket: CheckBucketSkip}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.check.Failing(); got != tt.wantFailing {
				t.Errorf("Failing() = %v, want %v", got, tt.wantFailing)
			}
			if got := tt.check.Pending(); got != tt.wantPending {
				t.Errorf("Pending() = %v, want %v", got, tt.wantPending)
			}
		})
	}
}

func TestMergeableStateHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state        MergeableState
		wantConflict bool
		wantResolved bool
	}{
		{MergeableOK, false, true},
		{MergeableConflict, true, true},
		{MergeablePending, false, false},
		{MergeableUnknown, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.Conflict(); got != tt.wantConflict {
				t.Errorf("Conflict() = %v, want %v", got, tt.wantConflict)
			}
			if got := tt.state.Resolved(); got != tt.wantResolved {
				t.Errorf("Resolved() = %v, want %v", got, tt.wantResolved)
			}
		})
	}
}

func TestErrUnsupportedIsMatched(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("wrap: " + ErrUnsupported.Error())
	if errors.Is(wrapped, ErrUnsupported) {
		t.Fatal("wrapping via Error() should not satisfy errors.Is")
	}
	wrappedProperly := errors.Join(errors.New("context"), ErrUnsupported)
	if !errors.Is(wrappedProperly, ErrUnsupported) {
		t.Fatal("errors.Join should satisfy errors.Is")
	}
}

// fakeHost asserts Host interface compliance at compile time.
type fakeHost struct{}

var _ Host = (*fakeHost)(nil)

func (fakeHost) Provider() Provider              { return ProviderUnknown }
func (fakeHost) Capabilities() Capabilities      { return Capabilities{} }
func (fakeHost) Available(context.Context) error { return nil }

func (fakeHost) FindPR(context.Context, string, string) (*PR, error) {
	return nil, nil
}
func (fakeHost) CreatePR(context.Context, string, string, PRContent) (*PR, error) {
	return nil, nil
}
func (fakeHost) UpdatePR(context.Context, *PR, PRContent) (*PR, error) {
	return nil, nil
}
func (fakeHost) GetPRState(context.Context, *PR) (PRState, error) {
	return "", nil
}
func (fakeHost) GetChecks(context.Context, *PR) ([]Check, error) {
	return nil, nil
}
func (fakeHost) GetMergeableState(context.Context, *PR) (MergeableState, error) {
	return "", ErrUnsupported
}
func (fakeHost) FetchFailedCheckLogs(context.Context, *PR, string, string, []string) (string, error) {
	return "", ErrUnsupported
}
