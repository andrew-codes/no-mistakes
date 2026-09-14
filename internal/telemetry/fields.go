package telemetry

import "github.com/andrew-codes/no-mistakes/internal/types"

func StepName(name types.StepName) string {
	if name.IsCustomGate() {
		return "gate"
	}
	return string(name)
}
