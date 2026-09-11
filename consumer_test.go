package gats_test

import (
	"os/exec"
	"testing"
)

// TestSeparateModuleConsumersBuild builds examples/consumer, which is a module
// of its own that depends on this one through a replace directive.
//
// It is the only check that the point of the package layout actually holds:
// that a client repository and a server repository can each import their own
// side of gats and compile. Nothing inside this module can prove that, because
// in-module code may freely import internal packages and may name internal
// types in exported signatures — both of which an outside consumer cannot do.
func TestSeparateModuleConsumersBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a second module; skipped under -short")
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = "examples/consumer"

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building examples/consumer failed: %v\n%s", err, out)
	}
}
