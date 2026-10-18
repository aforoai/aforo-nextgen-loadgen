package server

import "testing"

func TestRunArgs_HighTPSFlagOnlyWhenAcknowledged(t *testing.T) {
	contains := func(args []string, want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	base := TriggerRequest{Scenario: "high-tps", Target: "local", DurationSecs: 30, Workers: 8}

	args := runArgs("high-tps", base, "manifest.json", "/tmp/out")
	if contains(args, acknowledgeHighTPSFlag) {
		t.Fatalf("unacknowledged request must not pass %s: %v", acknowledgeHighTPSFlag, args)
	}
	for _, want := range []string{"run", "--scenario", "high-tps", "--target", "local", "--manifest", "manifest.json", "--out", "/tmp/out", "--duration", "30s", "--workers", "8"} {
		if !contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}

	base.Acknowledge = true
	args = runArgs("high-tps", base, "manifest.json", "/tmp/out")
	if !contains(args, acknowledgeHighTPSFlag) {
		t.Fatalf("acknowledged request must pass %s: %v", acknowledgeHighTPSFlag, args)
	}
}
