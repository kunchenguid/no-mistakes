package authorization

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func managedContext(url string) Context {
	return Context{
		Managed:           true,
		VerifierURL:       url,
		Token:             "super-secret-token",
		TaskID:            "task-1",
		RuntimeGeneration: 7,
		SessionID:         "session-1",
		ProjectPath:       "/repo",
		Repository:        "github.com/acme/repo",
		WorktreePath:      "/repo-worktree",
		Branch:            "feature/auth",
		DurableMode:       "no-mistakes",
	}
}

func matchingResponse(request Request) Response {
	return Response{
		ProtocolVersion:   ProtocolVersion,
		RequestID:         request.RequestID,
		Operation:         request.Operation,
		TaskID:            request.TaskID,
		RuntimeGeneration: request.RuntimeGeneration,
		SessionID:         request.SessionID,
		ProjectPath:       request.ProjectPath,
		Repository:        request.Repository,
		WorktreePath:      request.WorktreePath,
		Branch:            request.Branch,
		DurableMode:       request.DurableMode,
		Allowed:           true,
		Reason:            "authorized",
	}
}

func TestAuthorizeAllowsOnlyExactEcho(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer super-secret-token" {
			t.Fatalf("authorization header = %q", got)
		}
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Repository != "github.com/acme/repo" {
			t.Fatalf("repository was not canonicalized: %q", request.Repository)
		}
		_ = json.NewEncoder(w).Encode(matchingResponse(request))
	}))
	defer server.Close()

	ctx := managedContext(server.URL)
	ctx.Repository = "https://user:repository-secret@github.com/acme/repo.git"
	if err := ctx.Authorize(context.Background(), OperationRun); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestAuthorizeFailsClosedOnMismatchDenialReplayAndMalformedResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Response)
		body   string
		status int
	}{
		{name: "protocol", mutate: func(r *Response) { r.ProtocolVersion = "v999" }},
		{name: "operation", mutate: func(r *Response) { r.Operation = OperationGatePush }},
		{name: "task", mutate: func(r *Response) { r.TaskID = "other" }},
		{name: "runtime", mutate: func(r *Response) { r.RuntimeGeneration++ }},
		{name: "session", mutate: func(r *Response) { r.SessionID = "other" }},
		{name: "project", mutate: func(r *Response) { r.ProjectPath = "/other" }},
		{name: "repository", mutate: func(r *Response) { r.Repository = "github.com/acme/other" }},
		{name: "worktree", mutate: func(r *Response) { r.WorktreePath = "/other" }},
		{name: "branch", mutate: func(r *Response) { r.Branch = "other" }},
		{name: "mode", mutate: func(r *Response) { r.DurableMode = "direct-PR" }},
		{name: "replay", mutate: func(r *Response) { r.RequestID = "already-used" }},
		{name: "denied direct-pr", mutate: func(r *Response) {
			r.Allowed = false
			r.DurableMode = "direct-PR"
			r.Reason = "durable_task_mode_denied"
		}, status: http.StatusForbidden},
		{name: "denied local-only", mutate: func(r *Response) {
			r.Allowed = false
			r.DurableMode = "local-only"
			r.Reason = "durable_task_mode_denied"
		}, status: http.StatusForbidden},
		{name: "untrusted reason", mutate: func(r *Response) {
			r.Allowed = false
			r.Reason = "super-secret-token"
		}, status: http.StatusForbidden},
		{name: "malformed", body: "not-json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request Request
				_ = json.NewDecoder(r.Body).Decode(&request)
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
					return
				}
				response := matchingResponse(request)
				tt.mutate(&response)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			err := managedContext(server.URL).Authorize(context.Background(), OperationRun)
			if err == nil {
				t.Fatal("expected denial")
			}
			if strings.Contains(err.Error(), "super-secret-token") {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}

func TestAuthorizeFailsClosedOnMissingVerifierAndTimeout(t *testing.T) {
	ctx := managedContext("")
	if err := ctx.Authorize(context.Background(), OperationRun); err == nil {
		t.Fatal("missing verifier should fail")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	ctx = managedContext(server.URL)
	ctx.Timeout = 10 * time.Millisecond
	if err := ctx.Authorize(context.Background(), OperationRun); err == nil {
		t.Fatal("timeout should fail")
	}
}

func TestAuthorizeDoesNotForwardCredentialAcrossRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if got := r.Header.Get("authorization"); got != "" {
			t.Fatalf("redirect forwarded credential: %q", got)
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	err := managedContext(source.URL).Authorize(context.Background(), OperationRun)
	if err == nil {
		t.Fatal("redirected verifier should fail closed")
	}
	if redirected {
		t.Fatal("verifier redirect was followed")
	}
}

func TestUnmanagedAuthorizeHasNoNetworkSideEffect(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	ctx := Context{VerifierURL: server.URL}
	if err := ctx.Authorize(context.Background(), OperationRun); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unmanaged authorization contacted verifier")
	}
}

func TestValidateLocalScopeRejectsNonManagedDurableModesBeforeVerifier(t *testing.T) {
	for _, mode := range []string{"direct-PR", "local-only"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
			defer server.Close()
			ctx := managedContext(server.URL)
			ctx.DurableMode = mode
			if err := ctx.ValidateLocalScope("/repo", "github.com/acme/repo", "/repo-worktree", "feature/auth"); err == nil {
				t.Fatal("non-no-mistakes durable mode should fail local validation")
			}
			if called {
				t.Fatal("local durable-mode rejection contacted verifier")
			}
		})
	}
}

func TestFromEnvironmentManagedPerchAndGenericModes(t *testing.T) {
	perch := []string{
		"PERCH_TASK_ID=task-1",
		"PERCH_TASK_MODE=no-mistakes",
		"PERCH_TASK_PROJECT=/repo",
		"PERCH_TASK_REPOSITORY=github.com/acme/repo",
		"PERCH_TASK_WORKTREE=/repo-wt",
		"PERCH_TASK_BRANCH=feature/auth",
		"PERCH_RUNTIME_GENERATION=7",
		"PERCH_SESSION_ID=session-1",
		"PERCH_HOOK_URL=http://127.0.0.1:8787/hooks",
		"PERCH_HOOK_TOKEN=secret",
	}
	ctx, err := FromEnvironment(perch)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Managed || ctx.VerifierURL != "http://127.0.0.1:8787/hooks/no-mistakes/authorize" {
		t.Fatalf("unexpected Perch context: %#v", ctx.Redacted())
	}

	generic := []string{
		"NO_MISTAKES_AUTHORIZATION_MODE=managed",
		"NO_MISTAKES_AUTHORIZATION_URL=http://127.0.0.1/auth",
		"NO_MISTAKES_AUTHORIZATION_TOKEN=secret",
		"NO_MISTAKES_AUTHORIZATION_TASK_ID=task-1",
		"NO_MISTAKES_AUTHORIZATION_RUNTIME_GENERATION=7",
		"NO_MISTAKES_AUTHORIZATION_SESSION_ID=session-1",
		"NO_MISTAKES_AUTHORIZATION_PROJECT=/repo",
		"NO_MISTAKES_AUTHORIZATION_REPOSITORY=github.com/acme/repo",
		"NO_MISTAKES_AUTHORIZATION_WORKTREE=/repo-wt",
		"NO_MISTAKES_AUTHORIZATION_BRANCH=feature/auth",
		"NO_MISTAKES_AUTHORIZATION_DURABLE_MODE=no-mistakes",
	}
	if ctx, err = FromEnvironment(generic); err != nil || !ctx.Managed {
		t.Fatalf("generic context = %#v, %v", ctx.Redacted(), err)
	}
}

func TestFromEnvironmentRejectsPartialManagedContext(t *testing.T) {
	for _, env := range [][]string{
		{"PERCH_TASK_ID=task-1"},
		{"PERCH_HOOK_TOKEN=secret"},
		{"NO_MISTAKES_AUTHORIZATION_MODE=managed"},
	} {
		if _, err := FromEnvironment(env); err == nil {
			t.Fatalf("partial context should fail: %v", env)
		}
	}
}

func TestRedactedRepresentationsNeverExposeToken(t *testing.T) {
	ctx := managedContext("http://127.0.0.1/auth")
	for _, rendered := range []string{ctx.String(), ctx.GoString(), ctx.Redacted().String(), ctx.Wire().String(), ctx.Wire().GoString()} {
		if strings.Contains(rendered, ctx.Token) {
			t.Fatalf("rendered context leaked token: %s", rendered)
		}
	}
}
