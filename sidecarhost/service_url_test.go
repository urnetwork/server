package sidecarhost

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// A deployed block's URL comes from the environment its fragment gives it, read when the flag is read; a flag given
// on the command line wins over it, an empty one included; and a process with neither has none.
func TestServiceUrlFlagDefaultsToTheEnvironmentAndAGivenFlagWins(t *testing.T) {
	newFlags := func() (*flag.FlagSet, *ServiceUrlFlag) {
		flags := flag.NewFlagSet("block", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		return flags, NewServiceUrlFlag(flags, "api_url", ApiUrlEnvName, "api's base URL")
	}

	t.Setenv(ApiUrlEnvName, "")
	flags, apiUrl := newFlags()
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if apiUrl.Url() != "" {
		t.Fatalf("with no flag and no environment: %q", apiUrl.Url())
	}

	// read when the flag is read, so the environment the process runs with answers
	t.Setenv(ApiUrlEnvName, "https://api.example.com")
	if apiUrl.Url() != "https://api.example.com" || apiUrl.String() != "https://api.example.com" {
		t.Fatalf("the environment's default: %q", apiUrl.Url())
	}

	flags, apiUrl = newFlags()
	if err := flags.Parse([]string{"-api_url=https://api.other.example"}); err != nil {
		t.Fatal(err)
	}
	if apiUrl.Url() != "https://api.other.example" {
		t.Fatalf("a given flag: %q", apiUrl.Url())
	}
	flags, apiUrl = newFlags()
	if err := flags.Parse([]string{"-api_url="}); err != nil {
		t.Fatal(err)
	}
	if apiUrl.Url() != "" {
		t.Fatalf("an empty given flag: %q", apiUrl.Url())
	}

	// the usage names the variable
	usage := flags.Lookup("api_url").Usage
	if !strings.Contains(usage, "$"+ApiUrlEnvName) {
		t.Fatalf("the usage %q", usage)
	}
	// the flag package reads a zero value's String for the defaults it prints
	if (&ServiceUrlFlag{}).String() != "" || (*ServiceUrlFlag)(nil).Url() != "" {
		t.Fatal("a zero flag answered a value")
	}
}

// What each block's test holds its URL flags to: the flag is a ServiceUrlFlag reading the given variable.
func TestCheckServiceUrlFlagHoldsTheBinding(t *testing.T) {
	flags := flag.NewFlagSet("block", flag.ContinueOnError)
	NewServiceUrlFlag(flags, "api_url", ApiUrlEnvName, "api's base URL")
	NewServiceUrlFlag(flags, "sockets_url", SocketsUrlEnvName, "the sockets service's base URL")
	flags.String("plain_url", "", "a URL with a fixed default")
	if err := CheckServiceUrlFlag(flags, "api_url", ApiUrlEnvName); err != nil {
		t.Fatal(err)
	}
	if err := CheckServiceUrlFlag(flags, "sockets_url", SocketsUrlEnvName); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		envName string
	}{
		{name: "api_url", envName: SocketsUrlEnvName},
		{name: "plain_url", envName: ApiUrlEnvName},
		{name: "missing_url", envName: ApiUrlEnvName},
	} {
		if err := CheckServiceUrlFlag(flags, c.name, c.envName); err == nil {
			t.Errorf("-%s held to $%s", c.name, c.envName)
		}
	}
}
