package daemon

import (
	"context"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShutdown_DrainsLaunchAlreadyInSetup(t *testing.T) {
	p, database := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, database, "drain-setup")
	setup := make(chan struct{})
	releaseSetup := make(chan struct{})
	step := &drainStep{started: make(chan struct{}), release: make(chan struct{})}
	manager := NewRunManager(database, p, func() []pipeline.Step {
		close(setup)
		<-releaseSetup
		return []pipeline.Step{step}
	})
	type launchResult struct {
		id  string
		err error
	}
	launched := make(chan launchResult, 1)
	go func() {
		id, err := manager.startRun(context.Background(), repo, "main", head, head, "test", nil, "drain", "", false, "", nil)
		launched <- launchResult{id, err}
	}()
	select {
	case <-setup:
	case <-time.After(10 * time.Second):
		t.Fatal("launch did not reach setup")
	}
	stopped := make(chan struct{})
	go func() { manager.Shutdown(); close(stopped) }()
	deadline := time.Now().Add(5 * time.Second)
	for !manager.shuttingDown.Load() {
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("shutdown lost track of an admitted launch")
	default:
	}
	close(releaseSetup)
	var result launchResult
	select {
	case result = <-launched:
		if result.err != nil {
			t.Fatal(result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launch did not return")
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	select {
	case <-step.started:
		t.Fatal("late-registered executor began a step during drain")
	default:
	}
	run, err := database.GetRun(result.id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCancelled || run.Error == nil || *run.Error != "daemon shutting down" {
		t.Fatalf("late-registered run = %+v", run)
	}
}

func TestShutdown_ServiceAndClientBudgetsPermitDrain(t *testing.T) {
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "")
	p := paths.WithRoot(t.TempDir())
	unit := renderSystemdUnitWithProxyEnv("/opt/no-mistakes", p, "/home/test", nil)
	// mixed sends initial SIGTERM only to the daemon, unlike control-group,
	// which would kill its executing agents before the daemon can drain them.
	if systemdSectionValue(unit, "Service", "KillMode") != "mixed" || systemdSectionValue(unit, "Service", "TimeoutStopSec") != "65" {
		t.Fatalf("service does not permit bounded drain: %s", unit)
	}
	plist := renderLaunchAgentWithProxyEnv("/opt/no-mistakes", p, "/home/test", nil)
	decoder := xml.NewDecoder(strings.NewReader(plist))
	var key string
	var exitTimeout int
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			if err := decoder.DecodeElement(&key, &start); err != nil {
				t.Fatal(err)
			}
		} else if key == "ExitTimeOut" && start.Name.Local == "integer" {
			if err := decoder.DecodeElement(&exitTimeout, &start); err != nil {
				t.Fatal(err)
			}
		}
	}
	if exitTimeout != 65 {
		t.Fatalf("launchd ExitTimeOut = %d, want 65", exitTimeout)
	}
	if got := daemonStopTimeout(); got < 65*time.Second {
		t.Fatalf("stop client deadline = %v, want at least 65s", got)
	}
}
