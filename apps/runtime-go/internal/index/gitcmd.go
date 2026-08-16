package index

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout bounds a single git invocation. A repository with a broken
// core.fsmonitor, a stuck index.lock, or a hostile filter driver must not hang
// the tool forever — PLAN.md section 12 names an unbounded wait as the one
// known defect not to repeat.
const gitTimeout = 30 * time.Second

// hardenedConfig is prepended to every git invocation.
//
// **This is a security control, not tidiness.** A repository's own
// `.git/config` is attacker-controlled whenever the repository is someone
// else's — a clone, a fork's pull request, a CI checkout — and several git
// config keys name programs that git executes. `core.fsmonitor` is the proven
// one: `git check-ignore` refreshes the index, which runs it, so merely
// pointing plumb at a hostile tree executed its command. `-c` on the command
// line overrides the repository file, which environment scrubbing cannot do.
//
// `safe.directory` is no protection here: it triggers on an ownership
// mismatch, and a CI checkout is owned by the user running the job.
var hardenedConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.sshCommand=false",
	"-c", "diff.external=",
	"-c", "protocol.ext.allow=never",
	"-c", "uploadpack.packObjectsHook=",
}

// gitCommand builds a git invocation that ignores every configuration source
// the inspected repository could control.
//
// Every git call in this package must go through here. Constructing
// exec.Command("git", ...) directly re-opens the execution hole above.
func gitCommand(ctx context.Context, root string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)

	full := make([]string, 0, len(hardenedConfig)+len(args)+2)
	full = append(full, "-C", root)
	full = append(full, hardenedConfig...)
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	// CommandContext kills the direct child only. A git that leaves a
	// background grandchild holding the stdout pipe keeps Wait blocked on the
	// I/O copy forever — measured at 98s against a 30s cap. WaitDelay bounds
	// that copy after the process is signalled.
	cmd.WaitDelay = 5 * time.Second
	// Never inherit a terminal: a config-driven credential or pager prompt
	// would otherwise block on stdin forever.
	cmd.Stdin = nil
	return cmd, cancel
}

// gitEnv returns the environment for every git subprocess.
//
// The ambient environment must not decide what this tool reports. GIT_DIR and
// friends redirect the whole history — running under `git rebase --exec` or a
// hook would silently index a different repository — and a user's global config
// contributes core.excludesFile, which changes which paths count as drift from
// one machine to the next.
func gitEnv() []string {
	drop := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_CONFIG=", "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		"GIT_CONFIG_COUNT=", "GIT_EXEC_PATH=", "GIT_CEILING_DIRECTORIES=",
		// Inert only because GIT_CONFIG_COUNT is dropped and git gates on it.
		// Leaving the payload loaded next to the trigger is not a defence.
		"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_",
	}

	out := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		skip := false
		for _, prefix := range drop {
			if strings.HasPrefix(kv, prefix) {
				skip = true
				break
			}
		}
		// GIT_CONFIG_PARAMETERS carries -c settings from an enclosing git
		// process and is applied like config; it must go too.
		if strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			skip = true
		}
		if !skip {
			out = append(out, kv)
		}
	}

	return append(out,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_ASKPASS=",
	)
}
