package bluesky

import (
	"strings"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// The Jetstream wire, and the projection of it onto the broker-agnostic bridge.Message. No I/O lives
// in this file: everything here is a pure function of a decoded frame, which is what lets the whole
// mapping be table-tested without a websocket.
//
// The wire decoded here is Jetstream's v1 shape, which the v2 servers keep frozen on /subscribe for
// existing consumers. The one v2 addition worth having is the `cursor` field; v1-era servers omit it
// and TimeUS is the fallback.

// Collections this bridge understands. Anything else that arrives is filtered, which matters because
// #account and #identity frames are delivered unconditionally whatever wantedCollections says.
const (
	CollectionPost   = "app.bsky.feed.post"
	CollectionLike   = "app.bsky.feed.like"
	CollectionRepost = "app.bsky.feed.repost"
)

// Commit operations.
const (
	operationCreate = "create"
	operationUpdate = "update"
	operationDelete = "delete"
)

// kindCommit is the only frame kind that carries a record. Jetstream also emits `identity`,
// `account` and (on v2) `sync`, and may add more - the decoder tolerates all of them.
const kindCommit = "commit"

// Header keys projected onto bridge.Message.Headers.
//
// These ARE the interface between the adapter and the Transformer: the adapter decodes each frame
// once and publishes what it found here, so the Transformer stays a pure function of
// Subject+Data+Headers and needs no JSON parser of its own. They are also what --metadata-header
// selects from, so they are written in the service's metadata charset already (lowercase, hyphens)
// and survive normalisation unchanged.
const (
	hdrDID         = "did"
	hdrRkey        = "rkey"
	hdrURI         = "uri"
	hdrCID         = "cid"
	hdrCollection  = "collection"
	hdrOperation   = "operation"
	hdrLang        = "lang"
	hdrLangs       = "langs"
	hdrEmbed       = "embed"
	hdrExternalURI = "external-uri"
	hdrReplyRoot   = "reply-root"
	hdrReplyParent = "reply-parent"
	hdrSubject     = "subject"
	hdrCreatedAt   = "created-at"

	// hdrHandle is set only on the feed path: a firehose commit carries the author's DID but not
	// their handle, whereas a hydrated feed post carries both. It is worth having because a DID is
	// unreadable and a handle names the news organisation.
	hdrHandle = "handle"
)

// event is one Jetstream frame.
type event struct {
	DID    string  `json:"did"`
	TimeUS int64   `json:"time_us"`
	Cursor int64   `json:"cursor"`
	Kind   string  `json:"kind"`
	Commit *commit `json:"commit,omitempty"`
}

// commit is a repository mutation: one record created, updated or deleted.
type commit struct {
	Rev        string  `json:"rev"`
	Operation  string  `json:"operation"`
	Collection string  `json:"collection"`
	Rkey       string  `json:"rkey"`
	CID        string  `json:"cid,omitempty"`
	Record     *record `json:"record,omitempty"`
}

// record is the union of the three lexicons this bridge understands, decoded into one struct rather
// than three. Their fields are disjoint - a post has text, a like has a subject - so one decode of a
// small object is cheaper and simpler than dispatching to a second Unmarshal per collection.
type record struct {
	Type      string   `json:"$type"`
	Text      string   `json:"text,omitempty"`
	CreatedAt string   `json:"createdAt,omitempty"`
	Langs     []string `json:"langs,omitempty"`
	Reply     *reply   `json:"reply,omitempty"`
	Embed     *embed   `json:"embed,omitempty"`
	Subject   *ref     `json:"subject,omitempty"`
}

type reply struct {
	Root   *ref `json:"root,omitempty"`
	Parent *ref `json:"parent,omitempty"`
}

type ref struct {
	URI string `json:"uri"`
	CID string `json:"cid,omitempty"`
}

type embed struct {
	Type string `json:"$type"`

	// External is the link card a news post almost always carries. Its URI is the single most
	// useful field on the record for relating posts to each other - see topics.go.
	External *external `json:"external,omitempty"`
}

type external struct {
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
}

// cursor is the resume point to report once this frame has been fully handled: the v2 sequence
// number when the server sent one, else the v1 ingest timestamp. The server tells the two apart by
// magnitude, so passing back whichever it gave us is always right.
func (e *event) cursor() int64 {
	if e.Cursor > 0 {
		return e.Cursor
	}

	return e.TimeUS
}

// uri builds the at:// URI that identifies a record - and, for a post, the id of the memory it
// becomes. That identity is the whole reason this bridge needs no state: a like names its target by
// this same string, so reinforcing it is a call, not a lookup.
func uri(did string, collection string, rkey string) string {
	return "at://" + did + "/" + collection + "/" + rkey
}

// toMessage normalises one Jetstream commit onto the broker-agnostic bridge.Message.
//
// Subject is the record's COLLECTION - one of a handful of closed values, so it is safe to use as a
// group or a metadata label, unlike anything derived from the account or the text.
//
// Data is the post's text and nothing else. Putting the raw frame in would write the DID, the CID
// and the whole record envelope into every memory body, which is not what a reader of the store
// wants to find there, and would defeat content search.
//
// Timestamp is the record's client-supplied createdAt, NOT the frame's time_us. time_us is
// Jetstream's own witness time - when the relay saw the record, not when it was written - so using
// it would put the store's decay clock on the relay's clock. createdAt is routinely wrong and
// routinely in the future, which is exactly what bridge.DefaultTransformer's existing clamp handles.
func toMessage(ev *event) bridge.Message {
	c := ev.Commit

	msg := bridge.Message{
		Subject: c.Collection,
		Headers: map[string]string{
			hdrDID:        ev.DID,
			hdrRkey:       c.Rkey,
			hdrURI:        uri(ev.DID, c.Collection, c.Rkey),
			hdrCollection: c.Collection,
			hdrOperation:  c.Operation,
		},
	}

	if c.CID != "" {
		msg.Headers[hdrCID] = c.CID
	}

	// A delete carries no record at all, so everything below is absent by construction.
	if c.Record == nil {
		return msg
	}

	r := c.Record

	msg.Data = []byte(r.Text)

	if r.CreatedAt != "" {
		msg.Headers[hdrCreatedAt] = r.CreatedAt

		if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
			msg.Timestamp = t
		}
	}

	if len(r.Langs) > 0 {
		msg.Headers[hdrLang] = r.Langs[0]
		msg.Headers[hdrLangs] = strings.Join(r.Langs, ",")
	}

	if r.Embed != nil {
		if r.Embed.Type != "" {
			msg.Headers[hdrEmbed] = r.Embed.Type
		}

		if r.Embed.External != nil && r.Embed.External.URI != "" {
			msg.Headers[hdrExternalURI] = r.Embed.External.URI
		}
	}

	if r.Reply != nil {
		if r.Reply.Root != nil {
			msg.Headers[hdrReplyRoot] = r.Reply.Root.URI
		}

		if r.Reply.Parent != nil {
			msg.Headers[hdrReplyParent] = r.Reply.Parent.URI
		}
	}

	if r.Subject != nil && r.Subject.URI != "" {
		msg.Headers[hdrSubject] = r.Subject.URI
	}

	return msg
}
