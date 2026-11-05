// Only the existing closed Apport argument grammar is accepted. No flag can
// select configuration, registry, executable, interpreter or a host pathname.
package competitioncore

import (
	"errors"
	"strconv"
	"strings"
)

// Kernel placeholders are already expanded when this parser is called.
func ParseInvocation(arguments []string, settings Settings) (Invocation, error) {
	result := Invocation{}
	if len(arguments) != 10 || arguments[8] != "--" || settings.ArgumentBytes <= 0 || settings.MaximumDescriptor == 0 {
		return result, errors.New("unexpected core invocation grammar")
	}
	for _, argument := range arguments {
		if len(argument) > settings.ArgumentBytes || strings.ContainsRune(argument, 0) {
			return result, errors.New("core invocation exceeds its finite argument grammar")
		}
	}
	values := []*uint64{&result.NamespacePid, &result.Signal, &result.CoreLimit, &result.DumpMode, &result.HostPid, &result.Uid, &result.Gid}
	for index, prefix := range []string{"-p", "-s", "-c", "-d", "-P", "-u", "-g"} {
		if !strings.HasPrefix(arguments[index], prefix) {
			return Invocation{}, errors.New("core invocation reordered its fixed arguments")
		}
		value := strings.TrimPrefix(arguments[index], prefix)
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || strconv.FormatUint(parsed, 10) != value {
			return Invocation{}, errors.New("core invocation has a noncanonical numeric argument")
		}
		*values[index] = parsed
	}
	if result.HostPid == 0 || result.HostPid > 2147483647 || result.NamespacePid == 0 || result.NamespacePid > 2147483647 ||
		result.Signal == 0 || result.Signal > 64 || result.DumpMode > 2 || result.Uid > 4294967295 || result.Gid > 4294967295 ||
		!strings.HasPrefix(arguments[7], "-F") || arguments[9] == "" {
		return Invocation{}, errors.New("core invocation is outside the fixed kernel contract")
	}
	result.Descriptor = strings.TrimPrefix(arguments[7], "-F")
	if result.Descriptor != "" {
		fd, err := strconv.ParseUint(result.Descriptor, 10, 64)
		if err != nil || strconv.FormatUint(fd, 10) != result.Descriptor || fd > settings.MaximumDescriptor {
			return Invocation{}, errors.New("core descriptor argument is not finite")
		}
	}
	result.Executable, result.arguments = arguments[9], append([]string(nil), arguments...)
	return result, nil
}

// The vector retains even an absent older-kernel %F expansion exactly. A fresh
// fixed environment is supplied separately; no shell interprets these values.
func (self Invocation) ApportArguments() []string { return append([]string(nil), self.arguments...) }
