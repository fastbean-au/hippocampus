package bluesky

import (
	"container/list"
	"net/url"
	"sync"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// Relating posts to each other, without any NLP.
//
// The trick is that a news post's link card carries the article URL, and a news URL's path is a
// SLUG someone wrote by hand: /politics/2026/08/samuel-alito-ethics-conflicts-interest-fossil. That
// is a keyword list, already tokenised on hyphens, chosen editorially rather than grammatically -
// which is exactly what relatedness wants and is strictly better here than extracting nouns from the
// headline would be. Splitting it costs a regexp-free scan; a part-of-speech tagger would cost a
// dependency, model data and per-post CPU to do the same job worse.
//
// Two posts are related when they share at least minSharedTerms of these, ignoring terms that turn
// up all over the corpus (those are section names - politics, world, news - not topics). Measured
// against a live news feed, that relates about a fifth of posts, and the matches are cross-outlet:
// one paper's "Alito made up to $2.9 million from fossil fuel assets" against another's "Supreme
// Court justice Samuel Alito gained up to $2.9 million from...".
//
// WHY BOTHER: links are what make consolidation.linkRecallPropagation do anything. With them, a like
// on one outlet's coverage pulls the others back from the threshold too, and linkSignificanceWeight
// makes a cluster of coverage more durable than a lone post - a real claim about news, demonstrated
// rather than asserted.

// minTermLength drops the short tokens that carry no topic ("the", "us", "2026" is caught
// separately). Four is empirical: it keeps "gaza" and "alito" and drops most noise.
const minTermLength = 4

// maxTermsPerPost bounds how many terms one post contributes, so a pathological URL cannot flood the
// index.
const maxTermsPerPost = 24

// stopTerms are the tokens a URL path carries that say nothing about the story: sections, formats,
// and the furniture of a CMS. Deliberately small - the document-frequency cap below does most of the
// work, and it does it in whatever language the feed happens to be in.
var stopTerms = map[string]bool{
	"news": true, "article": true, "articles": true, "story": true, "stories": true,
	"video": true, "videos": true, "live": true, "updates": true, "update": true,
	"html": true, "index": true, "amp": true, "utm": true, "http": true, "https": true,
	"www": true, "com": true, "co": true, "org": true, "net": true,
	"world": true, "politics": true, "business": true, "sport": true, "sports": true,
	"opinion": true, "comment": true, "analysis": true, "feature": true, "features": true,
	"content": true, "uploads": true, "wp": true, "amp4": true, "story-": true,
}

// extractTerms pulls the topic terms out of a message.
//
// The external URL's path is preferred, being editorially chosen. Only when there is no link card
// does it fall back to the post's own text - which is noisier, but is the difference between relating
// a tenth of the corpus and relating none of the posts that carry no link.
func extractTerms(msg bridge.Message) []string {
	if raw := msg.Headers[hdrExternalURI]; raw != "" {
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			if terms := tokenise(u.Path); len(terms) > 0 {
				return terms
			}
		}
	}

	return tokenise(string(msg.Data))
}

// tokenise lowercases, splits on anything that is not a letter or digit, and keeps the tokens long
// enough and distinctive enough to be worth indexing. Purely numeric tokens go: a URL path is full
// of dates, and two stories sharing "2026" are not related.
func tokenise(s string) []string {
	var (
		out   []string
		seen  = map[string]bool{}
		start = -1
	)

	// One pass over the bytes. Non-ASCII bytes are treated as word characters, so a term in another
	// script survives as a unit rather than being shredded into fragments.
	for i := 0; i <= len(s); i++ {
		var word bool

		if i < len(s) {
			c := s[i]
			word = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
		}

		if word {
			if start < 0 {
				start = i
			}

			continue
		}

		if start < 0 {
			continue
		}

		if term := normaliseTerm(s[start:i]); term != "" && !seen[term] {
			seen[term] = true
			out = append(out, term)

			if len(out) >= maxTermsPerPost {
				return out
			}
		}

		start = -1
	}

	return out
}

// normaliseTerm lowercases a token and reports "" for anything not worth indexing.
func normaliseTerm(tok string) string {
	if len(tok) < minTermLength {
		return ""
	}

	b := make([]byte, len(tok))
	digits := 0

	for i := range len(tok) {
		c := tok[i]

		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}

		if c >= '0' && c <= '9' {
			digits++
		}

		b[i] = c
	}

	// A purely numeric token is a date or an article id, never a topic.
	if digits == len(tok) {
		return ""
	}

	term := string(b)

	if stopTerms[term] {
		return ""
	}

	return term
}

// topicIndex maps a term to the memories that recently carried it.
//
// This is the first genuinely STATEFUL thing in this bridge, and unlike the roots cache it is not merely an
// optimisation: lose it and links stop being made. That is accepted deliberately - linking is
// best-effort enrichment, nothing depends on it being complete, and the alternative (asking the
// service which stored memories share a term) is a query per post for a feature worth one map
// lookup. It is bounded by memory count, so it forgets in roughly the order the store does.
type topicIndex struct {
	mu    sync.Mutex
	size  int
	order *list.List               // memory ids, oldest at the back
	terms map[string][]string      // term -> memory ids carrying it
	byID  map[string]*indexedEntry // memory id -> its terms and list position
}

type indexedEntry struct {
	terms   []string
	element *list.Element
}

func newTopicIndex(size int) *topicIndex {
	if size <= 0 {
		size = 1
	}

	return &topicIndex{
		size:  size,
		order: list.New(),
		terms: make(map[string][]string),
		byID:  make(map[string]*indexedEntry),
	}
}

// Related returns up to limit memory ids sharing at least minShared terms with the given ones,
// most-overlapping first.
//
// A term held by more than maxDocumentFrequency of the indexed memories is skipped: that is the
// cheap stand-in for IDF, and it is what stops a section name relating every post to every other.
func (x *topicIndex) Related(terms []string, minShared int, limit int, maxDocumentFrequency int) []string {
	if len(terms) == 0 || limit <= 0 {
		return nil
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	shared := make(map[string]int, len(terms)*2)

	for _, term := range terms {
		ids := x.terms[term]
		if len(ids) == 0 || len(ids) > maxDocumentFrequency {
			continue
		}

		for _, id := range ids {
			shared[id]++
		}
	}

	// Selection by count rather than a sort: the candidate set is small and this keeps the common
	// case (nothing shares enough) free.
	var out []string

	for count := len(terms); count >= minShared && len(out) < limit; count-- {
		for id, n := range shared {
			if n != count {
				continue
			}

			out = append(out, id)

			if len(out) >= limit {
				break
			}
		}
	}

	return out
}

// Add records a memory's terms, evicting the oldest memory when full.
func (x *topicIndex) Add(id string, terms []string) {
	if id == "" || len(terms) == 0 {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	if _, ok := x.byID[id]; ok {
		return
	}

	x.byID[id] = &indexedEntry{terms: terms, element: x.order.PushFront(id)}

	for _, term := range terms {
		x.terms[term] = append(x.terms[term], id)
	}

	for x.order.Len() > x.size {
		oldest := x.order.Back()
		if oldest == nil {
			break
		}

		x.evict(oldest.Value.(string))
	}
}

// evict removes one memory and every posting that named it. The caller holds x.mu.
func (x *topicIndex) evict(id string) {
	entry, ok := x.byID[id]
	if !ok {
		return
	}

	x.order.Remove(entry.element)
	delete(x.byID, id)

	for _, term := range entry.terms {
		ids := x.terms[term]

		for i, v := range ids {
			if v != id {
				continue
			}

			ids = append(ids[:i], ids[i+1:]...)

			break
		}

		if len(ids) == 0 {
			delete(x.terms, term)

			continue
		}

		x.terms[term] = ids
	}
}

// Len reports how many memories are indexed.
func (x *topicIndex) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()

	return x.order.Len()
}

// linksFor returns the links a memory should carry, and records it in the index for the posts that
// follow. The returned slice is nil when nothing is related, which is the common case.
func (b *Bridge) linksFor(msg bridge.Message, id string) []*contract.Link {
	if b.topics == nil || id == "" {
		return nil
	}

	terms := extractTerms(msg)
	if len(terms) == 0 {
		return nil
	}

	related := b.topics.Related(terms, b.cfg.TopicMinShared, b.cfg.TopicMaxLinks, b.topicFrequencyCap())

	// Indexed AFTER the lookup, so a post never relates to itself.
	b.topics.Add(id, terms)

	if len(related) == 0 {
		return nil
	}

	out := make([]*contract.Link, 0, len(related))

	for _, target := range related {
		out = append(out, &contract.Link{Id: target, Significance: b.cfg.TopicLinkSignificance})
	}

	return out
}

// topicFrequencyCap turns the configured percentage into a document count against what is currently
// indexed, with a floor so a small or freshly-started index does not treat every term as common.
func (b *Bridge) topicFrequencyCap() int {
	cap := b.topics.Len() * b.cfg.TopicMaxFrequencyPercent / 100

	if cap < minFrequencyCap {
		return minFrequencyCap
	}

	return cap
}

// minFrequencyCap is the floor for the document-frequency cutoff. Below this, a bridge that has just
// started would relate nothing at all, since every term is in 100% of a two-post index.
const minFrequencyCap = 8

// attachLinks relates a whole batch to itself and to what is already indexed, writing the links onto
// the memories rather than issuing a call each. Used by the backfill, whose import resolves
// intra-batch targets in a second pass.
func (b *Bridge) attachLinks(mems []feedMemory) {
	if b.topics == nil {
		return
	}

	for _, v := range mems {
		if v.Memory == nil {
			continue
		}

		if links := b.linksFor(v.Message, v.Memory.GetId()); len(links) > 0 {
			v.Memory.Links = links
		}
	}
}
