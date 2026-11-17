// Shell-boundary tests share extraction without importing scoring/model fixtures.
package main

import (
	"os"
	"strings"
	"testing"
)

// Extract a complete real function, never a rewritten copy of the predicate.
func g2ScriptFunction(t *testing.T, path, name string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := "\n" + name + "() {\n"
	start := strings.Index(string(content), marker)
	if start < 0 {
		t.Fatal("g2 script function is missing", name)
	}
	end := strings.Index(string(content[start+1:]), "\n}\n")
	if end < 0 {
		t.Fatal("g2 script function is incomplete", name)
	}
	return string(content[start+1 : start+1+end+3])
}
