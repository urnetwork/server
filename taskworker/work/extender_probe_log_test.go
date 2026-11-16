package work

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Root cause guarded: the probe named each address in its log lines, at default
// level when it deactivated one, which was harmless while every address was in
// the open tier. Once operator hosts are gated, those lines would put addresses
// no public channel carries into the logs. The lines name the extender by its id
// and family instead.
//
// This reads the task's source rather than its log output: the lines are what is
// pinned, including the ones behind a verbosity, which a run would not show, and
// a line added later is held to the same rule. The verdict, the control target
// and the way back are the task's too.
func TestExtenderProbeLogsNameNoAddress(t *testing.T) {
	logCallCount := 0
	for _, path := range []string{
		"extender_probe_work.go",
		"extender_probe_common_mode.go",
		"extender_probe_control.go",
		"extender_probe_recovery.go",
		"extender_probe_metrics.go",
	} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		inspectExtenderProbeLogCalls(t, file, &logCallCount)
	}
	if logCallCount == 0 {
		t.Fatal("no log line was read, so this pins nothing")
	}
}

// Holds every glog call of one file to the rule above.
func inspectExtenderProbeLogCalls(t *testing.T, file *ast.File, logCallCount *int) {
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if receiver, ok := selector.X.(*ast.Ident); !ok || receiver.Name != "glog" || selector.Sel.Name == "V" {
			return true
		}
		*logCallCount += 1
		for _, arg := range call.Args {
			// the scrubbed text is the one way a probe's error reaches a line
			if inner, ok := arg.(*ast.CallExpr); ok {
				if function, ok := inner.Fun.(*ast.Ident); ok && function.Name == "extenderProbeErrorText" {
					continue
				}
			}
			ast.Inspect(arg, func(argNode ast.Node) bool {
				switch argNode := argNode.(type) {
				case *ast.SelectorExpr:
					if argNode.Sel.Name == "Ip" {
						t.Errorf("a log line of the probe names an address: %s", selector.Sel.Name)
					}
				case *ast.Ident:
					if argNode.Name == "probeErr" {
						t.Errorf("a log line of the probe carries a dial's error unscrubbed: %s", selector.Sel.Name)
					}
				}
				return true
			})
		}
		return true
	})
}

// A failed dial names the address it dialed, in either family, and the text kept
// for the log says the rest of what happened without it.
func TestExtenderProbeErrorTextTakesTheAddressOut(t *testing.T) {
	for _, test := range []struct {
		ip       string
		err      string
		expected string
	}{
		{
			ip:       "192.0.2.1",
			err:      "dial tcp 192.0.2.1:443: i/o timeout",
			expected: "dial tcp <address>:443: i/o timeout",
		},
		{
			ip:       "2001:db8::1",
			err:      "dial tcp [2001:db8::1]:443: connect: connection refused",
			expected: "dial tcp [<address>]:443: connect: connection refused",
		},
		{
			ip:       "::ffff:192.0.2.1",
			err:      "dial tcp 192.0.2.1:443: i/o timeout, again ::ffff:192.0.2.1",
			expected: "dial tcp <address>:443: i/o timeout, again <address>",
		},
		{
			ip:       "192.0.2.1",
			err:      "the extender did not answer",
			expected: "the extender did not answer",
		},
	} {
		target := &model.NetworkExtenderProbeTarget{
			ExtenderId: server.NewId(),
			Ip:         netip.MustParseAddr(test.ip),
		}
		text := extenderProbeErrorText(target, errors.New(test.err))
		if text != test.expected {
			t.Errorf("%s became %q", test.err, text)
		}
		if strings.Contains(text, target.Ip.Unmap().String()) {
			t.Errorf("%q still names the address", text)
		}
	}
}
