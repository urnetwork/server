// Identifies the public Redis adapter called by a close-request taker in one
// recovered stack. Only the adapter name is retained in the panic exemplar.
package monitor

import (
	"strings"
	"unicode/utf8"
)

// The selected owner can be the named taker or its SPOP callback. In the
// latter case each intervening callback frame must belong to this same call.
func panicCloseRequestRedisAdapter(line, module string, stack []string, selected int) string {
	if !utf8.ValidString(line) {
		return ""
	}
	root := strings.TrimSuffix(module, "/model")
	const prefix = "github.com/urnetwork/"
	const take = "TakeClientCloseRequests"
	adapterAt, takeAt := -1, -1
	var adapter string
	for index, entry := range stack {
		symbol, _, _ := strings.Cut(strings.TrimSpace(entry), "(")
		switch symbol {
		case prefix + root + ".Redis", prefix + root + ".RedisDoOnce":
			if adapterAt >= 0 {
				return ""
			}
			adapterAt, adapter = index, strings.TrimPrefix(symbol, prefix+root+".")
		case prefix + module + "." + take:
			if takeAt >= 0 {
				return ""
			}
			takeAt = index
		}
	}
	frameIs := func(index int, pkg, name, file string) bool {
		if index < 0 || index+1 >= len(stack) {
			return false
		}
		function, source := strings.TrimSpace(stack[index]), strings.TrimSpace(stack[index+1])
		args, opened := strings.CutPrefix(function, prefix+pkg+"."+name+"(")
		args, closed := strings.CutSuffix(args, ")")
		location := panicProviderSourceRe.FindStringSubmatch(source)
		if !opened || !closed || strings.ContainsAny(args, "()\r\n") || strings.ContainsAny(source, "\r\n") ||
			len(location) != 3 || location[1] != file || location[2][0] == '0' ||
			(len(location[2]) == 7 && location[2] != "1000000") {
			return false
		}
		// A trimpath or module-cache path names its package as well as its
		// file. A checkout's absolute build path can supply only the basename.
		if _, path, qualified := strings.Cut(source, prefix); qualified {
			if versioned, ok := strings.CutPrefix(path, root+"@"); ok {
				version, relative, found := strings.Cut(versioned, "/")
				if !found || version == "" {
					return false
				}
				path = root + "/" + relative
			}
			return path == pkg+"/"+strings.TrimPrefix(location[0], "/")
		}
		return true
	}
	const modelFile = "client_close_model.go"
	if adapterAt < 0 || takeAt != adapterAt+2 || !frameIs(adapterAt, root, adapter, "redis.go") ||
		!frameIs(takeAt, module, take, modelFile) {
		return ""
	}
	if selected == takeAt {
		return adapter
	}
	if takeAt == selected+10 && frameIs(selected, module, take+".func1", modelFile) &&
		frameIs(selected+2, root, "redisWithClient.func1", "redis.go") &&
		frameIs(selected+4, root, "redisWithClient", "redis.go") &&
		frameIs(selected+6, root, adapter+".func1", "redis.go") {
		return adapter
	}
	return ""
}
