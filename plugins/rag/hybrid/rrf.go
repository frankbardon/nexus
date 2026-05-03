package hybrid

import "sort"

// candidate accumulates per-backend scores for one document ID across the
// fusion pass. Score is the running fused score; rank fields are kept for
// debugging / future weighted hybrids.
type candidate struct {
	id         string
	content    string
	metadata   map[string]string
	similarity float32
	lexical    float32
	sources    []string
	score      float32
}

// fuseRRF runs Reciprocal Rank Fusion over per-backend ranked lists. Lower
// rank ⇒ higher contribution. The classic formula:
//
//	score(d) = Σ_lists 1 / (k + rank_in_list(d))
//
// k is the smoothing constant (60 in the original paper, configurable via
// rrfK). Lists with bias > 1 contribute proportionally more; bias < 1 less.
func fuseRRF(lists []rankedList, rrfK float64) []candidate {
	pool := make(map[string]*candidate)
	for _, lst := range lists {
		for rank, doc := range lst.docs {
			c, ok := pool[doc.id]
			if !ok {
				c = &candidate{
					id:       doc.id,
					content:  doc.content,
					metadata: doc.metadata,
				}
				pool[doc.id] = c
			}
			contribution := lst.bias / (rrfK + float64(rank+1))
			c.score += float32(contribution)
			if doc.similarity != 0 && c.similarity == 0 {
				c.similarity = doc.similarity
			}
			if doc.lexical != 0 && c.lexical == 0 {
				c.lexical = doc.lexical
			}
			c.sources = appendUnique(c.sources, lst.source)
		}
	}
	out := make([]candidate, 0, len(pool))
	for _, c := range pool {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// rankedList is one backend's ordered hits feeding into fusion. source names
// the backend ("vector", "lexical") and bias scales its contribution; bias
// of 1.0 leaves the list at its native weight.
type rankedList struct {
	source string
	bias   float64
	docs   []rankedDoc
}

// rankedDoc is one backend hit normalized so RRF / weighted fusion treat
// vector and lexical results uniformly. similarity and lexical are kept
// separately for downstream reporting.
type rankedDoc struct {
	id         string
	content    string
	metadata   map[string]string
	similarity float32
	lexical    float32
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}
