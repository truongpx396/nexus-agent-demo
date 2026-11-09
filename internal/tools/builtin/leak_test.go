package builtin

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running.
// tests/contract/goleak_test.go requires this of every package that starts one.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
