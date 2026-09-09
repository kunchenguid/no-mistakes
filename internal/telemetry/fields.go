package telemetry

import "github.com/kunchenguid/no-mistakes/internal/types"

func StepName(name types.StepName) string {
	if name.IsCustomGate() {
		return "gate"
	}
	return string(name)
}
