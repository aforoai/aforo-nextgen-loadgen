package cli

import "testing"

// The trigger server (internal/server/runner.go acknowledgeHighTPSFlag)
// passes --i-know-what-im-doing to `aforo-loadgen run` for acknowledged
// high-TPS runs. Renaming the flag here without updating the server would
// make every acknowledged server run exit at startup.
func TestRunCommandHasHighTPSAcknowledgementFlag(t *testing.T) {
	cmd := newRunCommand(nil)
	f := cmd.Flags().Lookup("i-know-what-im-doing")
	if f == nil {
		t.Fatal("run command must define --i-know-what-im-doing (used by the trigger server)")
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("--i-know-what-im-doing must be a bool flag, got %s", f.Value.Type())
	}
}
