package ui

import "strings"

// levenshtein is the edit distance between a and b (the distance cobra's
// "Did you mean" uses, over bytes, case-insensitive).
func levenshtein(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Closest is the candidate nearest to name by cobra's rule: an edit
// distance of at most 2, or a candidate that starts with name. The first
// (in candidates' order) of the nearest wins. ok is false when nothing
// qualifies.
func Closest(name string, candidates []string) (best string, ok bool) {
	bestDist := -1
	lname := strings.ToLower(name)
	for _, c := range candidates {
		d := levenshtein(name, c)
		if d > 2 && !strings.HasPrefix(strings.ToLower(c), lname) {
			continue
		}
		if bestDist < 0 || d < bestDist {
			best, bestDist, ok = c, d, true
		}
	}
	return best, ok
}
