// Package chaos is the one deliberate exception to Enquoir's clean
// architecture: a kill switch the worker checks directly, with no
// port and no interface, because its only job is to end the process
// outright at an exact point - there is nothing to abstract. Nothing
// else in this codebase may call os.Exit.
package chaos

import (
	"log/slog"
	"os"
)

// EnvVar names the one kill point the worker will die at. Unset, or
// set to anything that never matches a Point this process checks,
// KillAt does nothing - that is the only behaviour this ever changes.
const EnvVar = "ENQUOIR_CHAOS_KILL_AT"

// Point is a named moment in the create-transfer sequence a chaos run
// can kill the worker at.
type Point string

const (
	BeforeLog             Point = "before_log"
	AfterLogBeforeCall    Point = "after_log_before_call"
	DuringCall            Point = "during_call"
	AfterCallBeforeCommit Point = "after_call_before_commit"
)

// KillAt exits the process immediately if EnvVar names point.
func KillAt(point Point) {
	if os.Getenv(EnvVar) != string(point) {
		return
	}
	slog.Default().Error("CHAOS: killing the process now", "kill_at", point)
	os.Exit(1)
}

// WarnIfArmed logs a loud warning once at startup if EnvVar is set to
// anything at all, so an armed worker is never mistaken for a normal
// one from its logs alone.
func WarnIfArmed(logger *slog.Logger) {
	point := os.Getenv(EnvVar)
	if point == "" {
		return
	}
	logger.Warn("CHAOS MODE ARMED: this process will exit hard at a kill point", "kill_at", point)
}
