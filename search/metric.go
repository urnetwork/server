package search

// "github.com/urnetwork/server/v2025"

// https://en.wikipedia.org/wiki/Levenshtein_distance
func EditDistance(a string, b string) int {
	// TODO only need to use O(MIN(n, m)) memory by using omly current and previous

	n := len(a) + 1
	m := len(b) + 1

	index := func(alen int, blen int) int {
		return alen + n*blen
	}
	table := make([]int, n*m)
	table[index(0, 0)] = 0
	for alen := 1; alen <= len(a); alen += 1 {
		table[index(alen, 0)] = alen
	}
	for blen := 1; blen <= len(b); blen += 1 {
		table[index(0, blen)] = blen
	}
	for alen := 1; alen <= len(a); alen += 1 {
		for blen := 1; blen <= len(b); blen += 1 {
			if a[alen-1] == b[blen-1] {
				table[index(alen, blen)] = table[index(alen-1, blen-1)]
			} else {
				table[index(alen, blen)] = 1 + min(
					table[index(alen-1, blen)],
					table[index(alen, blen-1)],
					table[index(alen-1, blen-1)],
				)
			}
		}
	}
	return table[index(len(a), len(b))]
}
