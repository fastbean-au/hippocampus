package bluesky

import (
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// maxIDLength mirrors the service's own id cap (types/memory.go). A did:plc: URI is about 71
// characters and fits comfortably; did:web: is unbounded and does not, so those posts are dropped
// here rather than being rejected by the service on every single message.
const maxIDLength = 128

// Thread modelling modes, the values --events accepts.
const (
	EventsNone   = "none"
	EventsThread = "thread"
)

// Options carries what the shared TransformConfig cannot express, because it is specific to this
// wire rather than to bridges in general.
type Options struct {
	// Events selects thread modelling: EventsThread puts a reply's memory in its thread root's
	// event, EventsNone leaves every post standalone.
	Events string

	// Langs, when non-empty, keeps only posts declaring one of these languages.
	Langs []string

	// MinTextBytes drops a post whose text is shorter than this. A post with no text at all is
	// always dropped, whatever this is set to.
	MinTextBytes int
}

// Transformer maps a Jetstream commit onto zero or one memory.
//
// It WRAPS bridge.DefaultTransformer rather than reimplementing it. Body truncation, metadata key
// normalisation and bounding, group resolution, significance, and the future-timestamp clamp are all
// already there and already tested, and a post's client-supplied createdAt is precisely the kind of
// timestamp that clamp exists for. What this adds is the two things the shared transformer cannot
// know: which frames become memories at all, and that a memory's id is the post's at:// URI.
type Transformer struct {
	inner *bridge.DefaultTransformer
	opts  Options
	langs map[string]bool
}

// NewTransformer builds a Transformer over the shared configuration plus the Bluesky-specific
// options.
func NewTransformer(cfg bridge.TransformConfig, opts Options) *Transformer {
	langs := make(map[string]bool, len(opts.Langs))

	for _, v := range opts.Langs {
		if v != "" {
			langs[v] = true
		}
	}

	return &Transformer{
		inner: bridge.NewDefaultTransformer(cfg),
		opts:  opts,
		langs: langs,
	}
}

// Transform yields one memory for a post being created or updated, and nothing for anything else.
//
// Yielding nothing is a deliberate drop, which bridge.Store records as the `filtered` outcome - so
// the engagement stream still appears in hippocampus.bridge.messages rather than vanishing from the
// message counter, and is separately counted as reinforcement in hippocampus.bridge.recalls.
func (t *Transformer) Transform(msg bridge.Message) ([]*contract.Memory, error) {
	if msg.Headers[hdrCollection] != CollectionPost {
		return nil, nil
	}

	if op := msg.Headers[hdrOperation]; op != operationCreate && op != operationUpdate {
		return nil, nil
	}

	// Every drop below has to happen BEFORE the inner transformer runs, because
	// DefaultTransformer.body substitutes its empty-body placeholder for an empty payload. That is
	// right for a broker heartbeat and wrong here: a post with no text is not a memory reading
	// "(empty message)", it is not a memory.
	if len(msg.Data) == 0 || len(msg.Data) < t.opts.MinTextBytes {
		return nil, nil
	}

	if !t.languageWanted(msg) {
		return nil, nil
	}

	id := msg.Headers[hdrURI]

	if len(id) > maxIDLength {
		log.WithField("uri", id).
			Warn("dropping a post whose at:// URI exceeds the service's id length limit")

		return nil, nil
	}

	mems, err := t.inner.Transform(msg)
	if err != nil {
		return nil, err
	}

	if len(mems) == 0 {
		return nil, nil
	}

	// The id is the URI, which is what makes a like reinforceable without a lookup and what makes a
	// replayed frame an idempotent AlreadyExists rather than a duplicate memory.
	mems[0].Id = id

	if t.opts.Events == EventsThread {
		if root := msg.Headers[hdrReplyRoot]; root != "" && len(root) <= maxIDLength {
			mems[0].EventId = root
		}
	}

	return mems, nil
}

// languageWanted reports whether the post declares one of the configured languages. A post
// declaring none is kept: langs is optional in the lexicon, and dropping untagged posts would
// silently discard a large slice of the firehose.
//
// DECLARES is the operative word, and the filter can be no better than the declaration. The record's
// langs field is set by whichever client posted, generally from the account's interface language
// rather than from the text, so French posts declaring "en" reach a --langs en bridge routinely and
// are then stored carrying lang=en. Detecting the language here instead would need a model and a
// per-message cost this module will not carry, and would disagree with the field the rest of the
// network filters on. The declaration is therefore passed through as it stands; see
// docs/eventsource.md.
func (t *Transformer) languageWanted(msg bridge.Message) bool {
	if len(t.langs) == 0 {
		return true
	}

	langs := msg.Headers[hdrLangs]
	if langs == "" {
		return true
	}

	for _, v := range splitCommas(langs) {
		if t.langs[v] {
			return true
		}
	}

	return false
}

// splitCommas splits the joined language list without pulling in strings.Split's empty-element
// behaviour for an empty input.
func splitCommas(s string) []string {
	var (
		out   []string
		start int
	)

	for i := range len(s) {
		if s[i] != ',' {
			continue
		}

		if i > start {
			out = append(out, s[start:i])
		}

		start = i + 1
	}

	if start < len(s) {
		out = append(out, s[start:])
	}

	return out
}
