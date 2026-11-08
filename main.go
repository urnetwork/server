// sidecarctl is the one binary for every operations command this build adds: the commands the plans
// put under bringyourctl or warpctl that did not exist yet (CONTRACTS.md C-23, owner direction of
// 2026-11-07). Commands that existed stay where they were, such as bringyourctl db migrate and
// warpctl build and deploy, and the customer-facing ur admin commands stay in ur.
//
// Each command is its own cmd_<name>.go, which registers itself from its init through
// registerSidecarctlCommand, so adding one never edits this file. The leading words of a command
// line choose the command, the longest registered match winning, and the command gets the arguments
// after them. A command fails by returning an error, which exits 1; a usage mistake exits 2, after
// the usage text. Callers such as a builder job read nothing but that exit status, so a command
// never reports a failure with exit 0.
//
// This is a nested module so that a command can import server's packages, and warp's, without the
// root module depending on either. It requires nothing yet. It stays one package, and no file names
// it by its module path, because the release's rewrite would turn that path into one under
// .../sidecar/v2026 that nothing provides (check/hygiene_modules_test.go). The change that first
// requires one of this repository's modules, connect or glog also adds sidecarctl to
// build/all/run.sh's sidecar_cli_release_edit, which edits cli's requires the same way.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// One operations command.
type sidecarctlCommand struct {
	// what selects it, as typed after sidecarctl, such as "certs swap"
	words string
	// what follows the words in its usage line
	usage string
	// one line on what it does
	summary string
	// runs it with the arguments after its words, printing its progress to out. An error from
	// sidecarctlUsageErrorf says the arguments were wrong.
	run func(args []string, out io.Writer) error
}

// The commands of one binary, by their words. Not safe for concurrent use; registration happens
// in init, before main runs.
type sidecarctlCommandSet struct {
	wordCommands map[string]*sidecarctlCommand
}

// An empty set.
func newSidecarctlCommandSet() *sidecarctlCommandSet {
	return &sidecarctlCommandSet{
		wordCommands: map[string]*sidecarctlCommand{},
	}
}

// Adds a command. Two commands under the same words would leave one unreachable, so that panics,
// as does a command with no words.
func (self *sidecarctlCommandSet) add(command *sidecarctlCommand) {
	words := strings.Join(strings.Fields(command.words), " ")
	if words == "" || words != command.words {
		panic(fmt.Sprintf("sidecarctl command words %q are not words separated by single spaces", command.words))
	}
	if _, taken := self.wordCommands[words]; taken {
		panic(fmt.Sprintf("two sidecarctl commands are registered as %q", words))
	}
	self.wordCommands[words] = command
}

// The command the leading arguments name, and the arguments after its words. The longest match
// wins, so "certs swap" is not taken for a "certs" command. Nil when nothing matches.
func (self *sidecarctlCommandSet) find(args []string) (*sidecarctlCommand, []string) {
	for wordCount := len(args); 0 < wordCount; wordCount -= 1 {
		if command, ok := self.wordCommands[strings.Join(args[:wordCount], " ")]; ok {
			return command, args[wordCount:]
		}
	}
	return nil, nil
}

// Every command's usage line and summary, sorted by words.
func (self *sidecarctlCommandSet) usageText() string {
	var text strings.Builder
	text.WriteString("Usage:\n")
	words := []string{}
	for commandWords := range self.wordCommands {
		words = append(words, commandWords)
	}
	slices.Sort(words)
	for _, commandWords := range words {
		command := self.wordCommands[commandWords]
		fmt.Fprintf(&text, "    sidecarctl %s %s\n        %s\n", command.words, command.usage, command.summary)
	}
	return text.String()
}

// Runs one command line and returns its exit code: 0 on success, 1 when the command failed, and 2
// for a command line no command takes, after the usage text.
func (self *sidecarctlCommandSet) run(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(stdout, self.usageText())
		return 0
	}
	command, commandArgs := self.find(args)
	if command == nil {
		fmt.Fprintf(stderr, "sidecarctl: no command is %q\n%s", strings.Join(args, " "), self.usageText())
		return 2
	}
	err := command.run(commandArgs, stdout)
	if err == nil {
		return 0
	}
	usageErr := &sidecarctlUsageError{}
	if errors.As(err, &usageErr) {
		fmt.Fprintf(stderr, "sidecarctl %s: %s\nUsage:\n    sidecarctl %s %s\n", command.words, usageErr.message, command.words, command.usage)
		return 2
	}
	fmt.Fprintf(stderr, "sidecarctl %s: %s\n", command.words, err)
	return 1
}

// Arguments a command cannot take, as opposed to a command that ran and failed.
type sidecarctlUsageError struct {
	message string
}

// The message alone; the caller adds the usage line.
func (self *sidecarctlUsageError) Error() string {
	return self.message
}

// A usage mistake, formatted.
func sidecarctlUsageErrorf(format string, args ...any) error {
	return &sidecarctlUsageError{message: fmt.Sprintf(format, args...)}
}

// Parses a command's arguments, where a flag may stand before, between or after the positionals,
// as warpctl's usage lines allow. A flag's value follows an =, since a value standing alone could
// not be told from a positional, and -- ends the flags. Returns the positionals in order.
func parseSidecarctlArgs(flags *flag.FlagSet, args []string) ([]string, error) {
	flags.SetOutput(io.Discard)
	positionals := []string{}
	flagArgs := []string{}
	for index, arg := range args {
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(flagArgs); err != nil {
		return nil, sidecarctlUsageErrorf("%v", err)
	}
	return positionals, nil
}

// The binary's commands, which each cmd_<name>.go's init adds to.
var sidecarctlCommands = newSidecarctlCommandSet()

// Adds a command to the binary. Call it from the init of the command's own cmd_<name>.go.
func registerSidecarctlCommand(command *sidecarctlCommand) {
	sidecarctlCommands.add(command)
}

// Runs the command line and exits with its code.
func main() {
	os.Exit(sidecarctlCommands.run(os.Args[1:], os.Stdout, os.Stderr))
}
