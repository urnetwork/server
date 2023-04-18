package search

import (
	"bringyour.com/bringyour"
)


func EditDistance(a string, b string) int {
	/*
	# https://en.wikipedia.org/wiki/Levenshtein_distance
        table = {}
        # fixme need to use the full string length
        k = min(len(a), len(b))

        table[(0, 0)] = 0
        for i in range(1, k+1):
            table[(i, 0)] = i
        for j in range(1, k+1):
            table[(0, j)] = j
        for i in range(1, k+1):
            for j in range(1, k+1):
                if a[i-1] == b[j-1]:
                    table[(i, j)] = table[(i - 1, j - 1)]
                else:
                    table[(i, j)] = 1 + min(
                        table[(i - 1, j)],
                        table[(i, j - 1)],
                        table[(i - 1, j - 1)]
                    )
        return table[(k, k)] + max(len(a) - k, len(b) - k)
    */

	index := func(alen int, blen int) int {
		return alen + len(a) * blen
	}
	table := make([]int, (len(a) + 1) * (len(b) + 1))
	table[index(0, 0)] = 0
	for alen := 1; alen <= len(a); alen += 1 {
		table[index(alen, 0)] = alen
	}
	for blen := 1; blen <= len(b); blen += 1 {
		table[index(0, blen)] = blen
	}
	for alen := 1; alen <= len(a); alen += 1 {
		for blen := 1; blen <= len(b); blen += 1 {
			if a[alen - 1] == b[blen - 1] {
				table[index(alen, blen)] = table[index(alen - 1, blen - 1)]
			} else {
				table[index(alen, blen)] = 1 + bringyour.MinInt(
					table[index(alen - 1, blen)],
					table[index(alen, blen - 1)],
					table[index(alen - 1, blen - 1)],
				)
			}
		}
	}
	return table[index(len(a), len(b))]
}
