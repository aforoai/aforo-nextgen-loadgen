// Package lifecycle is Session 6's subscription-state-machine driver.
//
// While the run engine (Session 4) sends events at a steady rate, this
// package fires lifecycle transitions in parallel — upgrades, downgrades,
// pause/resume, trial conversions, migrations, and payment retries.
// Together they exercise the platform's:
//
//   - 9-state subscription state machine (CREATED → … → CANCELLED)
//   - pro-ration math on offering migration
//   - rate plan version pinning across upgrades
//   - wallet hold/release lifecycle on PREPAID/HYBRID modes
//   - dunning escalation (PAST_DUE → SUSPEND/CANCEL after N retries)
//   - bill run concurrency vs in-flight migrations (Redis lock)
//
// Two design constraints are load-bearing:
//
//  1. Every transition is logged BEFORE the API call. If the agent hangs
//     on a slow endpoint, transitions.jsonl already records what we
//     attempted — no silent voids on shutdown.
//  2. Subscription sampling is deterministic from scenario.seed. A failing
//     run can be replayed bit-for-bit by re-running with the same seed.
//
// Sub-packages:
//
//   - Agent      — the orchestrator that schedules transitions during a run
//   - Transition — eight transition modules (upgrade, downgrade, …)
//   - Picker     — deterministic subscription sampler
//   - StateMachine — mirror of the platform's 9-state state machine
//   - TransitionLog — append-only JSONL writer
package lifecycle
