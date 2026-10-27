// An owned curl wrapper attests an early remote size refusal to this one
// native SSH invocation. Numeric SSH exit status alone is never that proof.
package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Fresh non-secret receipt framing is private and unrelated to settings.
const mimirShellBudgetPrefix = "MIMIR_OBSERVATION_CURL_BUDGET_V1 "

// Clone native arguments and forbid PTY stream merging/framing changes.
// Public/injected command strings and SSH identity arguments stay intact.
func mimirShellBudgetArgs(args []string, limit int) ([]string, string, error) {
	if len(args) == 0 || limit <= 0 {
		return nil, "", errors.New("Mimir observation command contract unavailable")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, "", errors.New("Mimir observation receipt unavailable")
	}
	marker := mimirShellBudgetPrefix + hex.EncodeToString(token) + " 63"
	bounded := append([]string{"-T"}, args...)
	bounded[len(bounded)-1] = mimirShellBudgetCommand(args[len(args)-1], marker, limit)
	return bounded, marker, nil
}

// FD9 retains the native diagnostic channel even for curl calls whose stderr
// is redirected or whose stdout is captured. The function preserves status,
// so existing failed-prefix and set-e behavior remains authoritative. Defining
// it only when curl exists preserves command-v prerequisite checks.
func mimirShellBudgetCommand(command, marker string, limit int) string {
	return "if command -v curl >/dev/null 2>&1; then\n" +
		"exec 9>&2\n" +
		"curl() {\n" +
		" mimir_observation_has_cap=0; mimir_observation_discard=0; mimir_observation_output_next=0\n" +
		" for mimir_observation_arg do\n" +
		"  if [ \"$mimir_observation_output_next\" -eq 1 ] && [ \"$mimir_observation_arg\" = /dev/null ]; then mimir_observation_discard=1; fi\n" +
		"  mimir_observation_output_next=0\n" +
		"  case \"$mimir_observation_arg\" in --max-filesize|--max-filesize=*) mimir_observation_has_cap=1;; -o|--output) mimir_observation_output_next=1;; --output=/dev/null) mimir_observation_discard=1;; esac\n" +
		" done\n" +
		" if [ \"$mimir_observation_has_cap\" -eq 0 ] && [ \"$mimir_observation_discard\" -eq 0 ]; then set -- --max-filesize " + strconv.Itoa(limit) + " \"$@\"; fi\n" +
		" if command curl \"$@\"; then return 0; else\n" +
		"  mimir_observation_curl_status=$?\n" +
		"  if [ \"$mimir_observation_curl_status\" -eq 63 ]; then printf '\\n%s\\n' " + shellSingleQuote(marker) + " >&9; fi\n" +
		"  return \"$mimir_observation_curl_status\"\n" +
		" fi\n" +
		"}\n" +
		"fi\n" + command
}

// One exact nonce-bound line proves the owned wrapper saw curl return63.
// Duplicate, foreign or damaged framing refuses observation without claiming
// a remote budget diagnosis. Unrelated output cannot supply a receipt.
func mimirShellBudgetReceipt(diagnostics, marker string) (bool, bool) {
	if marker == "" {
		return false, false
	}
	count := 0
	for _, line := range strings.SplitAfter(diagnostics, "\n") {
		if strings.Contains(line, mimirShellBudgetPrefix) {
			if line != marker+"\n" {
				return true, false
			}
			count++
		}
	}
	return count != 0, count == 1
}
