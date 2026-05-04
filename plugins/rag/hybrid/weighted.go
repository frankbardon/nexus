package hybrid

import "sort"

// fuseWeighted runs weighted-sum fusion over per-backend ranked lists. Each
// backend's scores are first min-max normalized into [0, 1] so vector
// similarity (typically 0..1 after cosine) and BM25 scores (range backend-
// dependent, can exceed 10) are commensurable. The fused score is then a
// weighted sum across backends; documents missing from a list contribute 0
// from that side.
//
// list bias is the multiplier on that backend's normalized contribution —
// e.g. {vector: 0.7, lexical: 0.3} from config.
func fuseWeighted(lists []rankedList) []candidate {
	pool := make(map[string]*candidate)

	for _, lst := range lists {
		if len(lst.docs) == 0 {
			continue
		}
		minScore, maxScore := scoreRange(lst)
		spread := maxScore - minScore
		for _, doc := range lst.docs {
			raw := docScore(doc, lst.source)
			var norm float32 = 1
			if spread > 0 {
				norm = (raw - minScore) / spread
			}
			contribution := norm * float32(lst.bias)

			c, ok := pool[doc.id]
			if !ok {
				c = &candidate{
					id:       doc.id,
					content:  doc.content,
					metadata: doc.metadata,
				}
				pool[doc.id] = c
			}
			c.score += contribution
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

// docScore extracts the per-backend raw score for normalization. Vector
// backend uses similarity; lexical uses the bm25 score (already flipped by
// the sqlite_fts provider so higher == better).
func docScore(doc rankedDoc, source string) float32 {
	if source == "lexical" {
		return doc.lexical
	}
	return doc.similarity
}

func scoreRange(lst rankedList) (float32, float32) {
	first := docScore(lst.docs[0], lst.source)
	min, max := first, first
	for _, d := range lst.docs[1:] {
		s := docScore(d, lst.source)
		if s < min {
			min = s
		}
		if s > max {
			max = s
		}
	}
	return min, max
}
