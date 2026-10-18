package driver

import (
	"os"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// envURLDriver describes a driver whose endpoint comes from an environment
// variable instead of the run's Target. Such drivers send traffic to a URL
// the target-based production guard would never see, so every one of them
// MUST be listed in envURLDrivers: the constructor reads its URL through
// envDriverURL (which only accepts listed variables), and the list is
// registered with aforo.GuardProduction at init so the guard checks the
// same URL the driver will use.
type envURLDriver struct {
	name   string // ingestion-path name, e.g. "ai_agent_rest"
	envVar string
}

// envURLDrivers is the single list of env-configured driver endpoints.
var envURLDrivers = []envURLDriver{
	{name: "ai_agent_rest", envVar: AIAgentRESTEnvURL},
	{name: "mcp_jsonrpc", envVar: MCPJsonRPCEnvURL},
	{name: "ai_agent_wire", envVar: AIAgentWireEnvURL},
}

func init() {
	aforo.RegisterDestinationSource(EnvDestinations)
}

// EnvDestinations returns the endpoint of every env-configured driver whose
// variable is set, labelled with the driver and variable name.
func EnvDestinations() []aforo.Destination {
	var out []aforo.Destination
	for _, d := range envURLDrivers {
		v := strings.TrimSpace(os.Getenv(d.envVar))
		if v == "" {
			continue
		}
		out = append(out, aforo.Destination{Source: d.name + " (" + d.envVar + ")", URL: v})
	}
	return out
}

// envDriverURL returns override when non-empty, otherwise the value of
// envVar. It panics if envVar is not in envURLDrivers: a driver reading an
// unlisted variable would bypass the production guard, and that is a
// programming error that must fail the first test that constructs it.
func envDriverURL(override, envVar string) string {
	if override != "" {
		return override
	}
	for _, d := range envURLDrivers {
		if d.envVar == envVar {
			return strings.TrimSpace(os.Getenv(envVar))
		}
	}
	panic("driver: env var " + envVar + " is not registered in envURLDrivers; the production guard would not see it")
}
