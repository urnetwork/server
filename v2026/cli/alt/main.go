package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/alt"
)

// Splits a comma separated option into its values. An absent or empty option
// leaves the list empty, which takes the default from the environment.
func splitOption(opts docopt.Opts, name string) []string {
	value, err := opts.String(name)
	if err != nil {
		return nil
	}
	values := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func main() {
	usage := `BringYour alt server. Serves the connect node and the API
directly on public UDP, with no load balancer in front.

Usage:
  alt [--port=<port>] [--h3-port=<h3port>] [--dns-port=<dnsport>]
      [--api-hosts=<apihosts>] [--connect-hosts=<connecthosts>]
      [--dns-tlds=<dnstlds>]
  alt -h | --help
  alt --version

Options:
  -h --help     Show this screen.
  --version     Show version.
  -p --port=<port>                Status listen port [default: 80].
  --h3-port=<h3port>              H3 listen port [default: 443].
  --dns-port=<dnsport>            Whodis listen port [default: 4053].
  --api-hosts=<apihosts>          Comma separated API server names. Defaults to the services config.
  --connect-hosts=<connecthosts>  Comma separated connect server names. Defaults to the services config.
  --dns-tlds=<dnstlds>            Comma separated whodis tlds. Defaults to the client tld.`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}
	port, err := opts.Int("--port")
	if err != nil {
		panic(err)
	}
	h3Port, err := opts.Int("--h3-port")
	if err != nil {
		panic(err)
	}
	dnsPort, err := opts.Int("--dns-port")
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	if err := alt.Run(ctx, alt.RunOptions{
		Port:         port,
		H3Port:       h3Port,
		DnsPort:      dnsPort,
		ApiHosts:     splitOption(opts, "--api-hosts"),
		ConnectHosts: splitOption(opts, "--connect-hosts"),
		DnsTlds:      splitOption(opts, "--dns-tlds"),
	}); err != nil {
		panic(err)
	}
}
