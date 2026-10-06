package daemon

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLateCIIPCRefusesAuthenticatedDaemonDescendant(t *testing.T) {
	p, _ := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestLateCIIPCDescendantHelper$")
	child.Env = append(os.Environ(), "NM_LATE_CI_TEST_SOCKET="+p.Socket())
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "nested_gate_context") {
		t.Fatalf("new mutation ingress bypassed ancestry guard: %s", output)
	}
}

func TestLateCIIPCDescendantHelper(t *testing.T) {
	socket := os.Getenv("NM_LATE_CI_TEST_SOCKET")
	if socket == "" {
		return
	}
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodRespondLateCI, &ipc.RespondParams{RunID: "nonexistent", Step: types.StepCI, Action: types.ActionFix, ExpectedHeadSHA: strings.Repeat("a", 40), AddedFindings: []types.Finding{{Description: "new requirement"}}}, nil)
	if err == nil {
		t.Fatal("nested late finding accepted")
	}
	fmt.Println(err)
}
