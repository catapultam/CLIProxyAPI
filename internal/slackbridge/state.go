package slackbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxReplies caps the remembered deliveries; the oldest go first. It is
	// large enough that a guest flood (rate limited) can't push a private
	// record out before its answer comes, which would otherwise be dropped.
	maxReplies = 10000
	// replyTTL matches the agentbus message TTL: a message older than this
	// has left the bus, so an answer to it goes to the session's own thread.
	replyTTL = 7 * 24 * time.Hour
	// maxDMLinks caps the remembered top-level DM messages; the oldest go
	// first. Links expire after replyTTL.
	maxDMLinks = 1000
	// dmLastTTL is how long a plain DM still goes to the agent the user last
	// talked to there.
	dmLastTTL = 7 * 24 * time.Hour
	// linkAbsentTTL is how long a linked conversation keeps its link while
	// the session is gone from the bus.
	linkAbsentTTL = 7 * 24 * time.Hour
)

var (
	errConfigUser = errors.New("user is set in config.yaml")
	errNotAllowed = errors.New("user is not allowed")
)

// replyRecord remembers where a message the bridge delivered came from, so
// the session it went to can answer in that Slack thread.
type replyRecord struct {
	ID       string `json:"id"`
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	// DMUser is the user whose DM with the bot Channel is, or empty for the
	// channel. An answer goes there only while they are still allowed.
	DMUser  string    `json:"dm_user,omitempty"`
	Session string    `json:"session"`
	At      time.Time `json:"at"`
	// TS is the user's Slack message itself, where its receipt shows as a
	// reaction. Records from before receipts have none.
	TS string `json:"ts,omitempty"`
	// Receipt is the receipt reaction on TS now: the queued one it was
	// delivered with, then reactionReceived, then reactionRead.
	Receipt string `json:"receipt,omitempty"`
	// Shown is the receipt reaction the bridge last put on TS and hasn't
	// taken off (syncReceipt); Receipt is where it is headed.
	Shown string `json:"shown,omitempty"`
	// Link marks a message that reached Session only because Channel was
	// linked to it (a guest's message, or the link notice). An answer goes
	// there only while Channel is still linked to Session.
	Link bool `json:"link,omitempty"`
	// TopLevel marks a message written at the top level of a conversation
	// other than the main channel (a DM, a group DM, another channel): an
	// answer to it is posted at the top level there too, not in a thread.
	// Records saved before it existed keep their ThreadTS routing.
	TopLevel bool `json:"top_level,omitempty"`
	// Command is the name of the command the message carried, so the
	// agent's report on it can be logged; empty for any other message.
	Command string `json:"command,omitempty"`
}

// convLink ties a whole Slack conversation other than the main channel (a
// group DM, another channel, a DM) to a session.
type convLink struct {
	Session string `json:"session"`
	// By is the owner who linked it.
	By string    `json:"by"`
	At time.Time `json:"at"`
	// Seen is when the session was last known to be on the bus. The link is
	// dropped once that is more than linkAbsentTTL ago.
	Seen time.Time `json:"seen"`
	// Kind is kindDM, kindGroup or kindChannel (empty in links saved before
	// it was recorded).
	Kind string `json:"kind,omitempty"`
	// Members are the user IDs known to be in the conversation: everyone a
	// chat or dm command opened it with, the owner who linked it in place,
	// and anyone who wrote there while it was linked (at most
	// maxLinkMembers). An owner can unlink every conversation a person is in.
	Members []string `json:"members,omitempty"`
}

// Conversation kinds of a link.
const (
	kindDM      = "dm"
	kindGroup   = "group"
	kindChannel = "channel"
	// maxLinkMembers caps the user IDs a link remembers.
	maxLinkMembers = 100
)

// linkedConv is a live link and the conversation it is for.
type linkedConv struct {
	Channel string
	convLink
}

// dmLink ties a top-level message in a DM to a session, so a thread reply
// under it reaches that session.
type dmLink struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Session string `json:"session"`
	// Agent marks a post the session made itself (rather than a user's
	// message delivered to it). A session's first one in a channel carries
	// its header line.
	Agent bool      `json:"agent,omitempty"`
	At    time.Time `json:"at"`
}

// dmLastEntry is the agent a user last talked to in their DM with the bot.
type dmLastEntry struct {
	Session string    `json:"session"`
	At      time.Time `json:"at"`
}

type allowedUser struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// config marks users seeded from allowed-emails; they are not persisted
	// and can't be removed from Slack.
	config bool
}

// threadRef is a session's home thread: the conversation it is in and its
// top message. Channel is empty only for a thread from a state file written
// before threads named their conversation; resolve fills in the channel.
type threadRef struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// UnmarshalJSON reads a threadRef, or a bare thread ts as older state files
// wrote it (Channel left empty).
func (r *threadRef) UnmarshalJSON(data []byte) error {
	var ts string
	if errTS := json.Unmarshal(data, &ts); errTS == nil {
		*r = threadRef{TS: ts}
		return nil
	}
	type plain threadRef
	var p plain
	if errJSON := json.Unmarshal(data, &p); errJSON != nil {
		return errJSON
	}
	*r = threadRef(p)
	return nil
}

// Where a session's home thread is: the main channel or an owner's DM.
const (
	homeChannel = "channel"
	homeDM      = "dm"
)

type stateFile struct {
	Threads map[string]threadRef `json:"threads"`
	// Homes maps a session id to where an owner moved its home thread
	// (homeChannel or homeDM); without an entry the config's home applies.
	Homes map[string]string `json:"homes,omitempty"`
	// HomeOwners maps a session id whose home an owner moved to their DM
	// (homeDM) to that owner's user ID, so the thread reopens there.
	HomeOwners map[string]string `json:"home_owners,omitempty"`
	// Links maps every thread ts linked to a session (not just the first
	// one in Threads) to its session id.
	Links   map[string]string `json:"links,omitempty"`
	Allowed []allowedUser     `json:"allowed"`
	// Replies lists delivered messages, oldest first.
	Replies []replyRecord `json:"replies,omitempty"`
	// DMLinks lists top-level DM messages tied to sessions, oldest first.
	DMLinks []dmLink `json:"dm_links,omitempty"`
	// DMLast maps a user ID to the agent they last talked to in their DM.
	DMLast map[string]dmLastEntry `json:"dm_last,omitempty"`
	// Conversations maps a linked conversation's channel ID to its link.
	Conversations map[string]convLink `json:"conversations,omitempty"`
	// Moved maps a session id that handed off (/clear, /resume, /branch) to
	// the session that took it over.
	Moved map[string]movedEntry `json:"moved,omitempty"`
	// Seen maps a session id the state routes to (threads, links, homes) to
	// when the bus last saw it, or when the bridge first noticed it; see
	// pruneSessions.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Approvals lists approval requests the bot posted, oldest first.
	Approvals []pendingApproval `json:"approvals,omitempty"`
}

// state holds session threads, delivered messages, DM routing and the
// allowlist. Changes to the allowlist and to conversation links are written
// through to disk at once (their callers report a failed save); every other
// change only marks the state dirty, and the bridge flushes it every
// flushEvery and on Stop.
type state struct {
	path     string
	now      func() time.Time
	mu       sync.Mutex
	threads  map[string]threadRef   // session id -> its home thread (agents post there)
	homes    map[string]string      // session id -> where an owner moved its home thread
	homeDMs  map[string]string      // session id -> the owner whose DM its home is (homeDM)
	sessions map[string]string      // thread ts -> session id, for every linked thread
	replies  []replyRecord          // delivered messages, oldest first, at most maxReplies
	dmLinks  []dmLink               // top-level DM messages, oldest first, at most maxDMLinks
	dmLasts  map[string]dmLastEntry // user ID -> the agent they last talked to in their DM
	convs    map[string]convLink    // channel ID -> the session the conversation is linked to
	users    []allowedUser          // config users first
	moved    map[string]movedEntry  // old session id -> the session that took it over
	seen     map[string]time.Time   // session id -> when it was last known on the bus
	// approvals are the approval requests the bot posted, oldest first, at
	// most maxApprovals; expired ones are pruned.
	approvals []pendingApproval
	// dirty marks changes not written to disk yet.
	dirty bool
	// early holds receipts for message ids not recorded yet (a waiter can
	// claim a message before deliver records it); record applies them. Not
	// persisted; earlyRing evicts the oldest past maxEarlyReceipts.
	early     map[string]string
	earlyRing []string
}

// loadState reads path; a missing file is an empty state. On a corrupt file it
// returns an empty state and the error.
func loadState(path string) (*state, error) {
	st := &state{path: path, now: time.Now, threads: map[string]threadRef{}, homes: map[string]string{}, homeDMs: map[string]string{}, sessions: map[string]string{}, dmLasts: map[string]dmLastEntry{}, convs: map[string]convLink{}, moved: map[string]movedEntry{}, seen: map[string]time.Time{}, early: map[string]string{}}
	if path == "" {
		return st, nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return st, nil
		}
		return st, errRead
	}
	var file stateFile
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		return st, errJSON
	}
	for sid, ref := range file.Threads {
		if sid == "" || ref.TS == "" {
			continue
		}
		st.threads[sid] = ref
		st.sessions[ref.TS] = sid
	}
	for sid, home := range file.Homes {
		if sid != "" && (home == homeChannel || home == homeDM) {
			st.homes[sid] = home
		}
	}
	for ts, sid := range file.Links {
		st.sessions[ts] = sid
	}
	for sid, owner := range file.HomeOwners {
		if sid != "" && owner != "" && st.homes[sid] == homeDM {
			st.homeDMs[sid] = owner
		}
	}
	// Expired entries stay until the next recordReply drops them; lookups
	// never return them.
	// A record of a top-level DM has no thread, but always a channel.
	for _, r := range file.Replies {
		if r.ID != "" && r.Session != "" && (r.ThreadTS != "" || r.Channel != "") {
			if r.Shown == "" && r.Receipt != receiptDismissed {
				// Saved before Shown: the receipt was applied.
				r.Shown = r.Receipt
			}
			st.replies = append(st.replies, r)
		}
	}
	if extra := len(st.replies) - maxReplies; extra > 0 {
		st.replies = st.replies[extra:]
	}
	for _, l := range file.DMLinks {
		if l.Channel != "" && l.TS != "" && l.Session != "" {
			st.dmLinks = append(st.dmLinks, l)
		}
	}
	if extra := len(st.dmLinks) - maxDMLinks; extra > 0 {
		st.dmLinks = st.dmLinks[extra:]
	}
	for user, e := range file.DMLast {
		if user != "" && e.Session != "" {
			st.dmLasts[user] = e
		}
	}
	// Links of sessions absent too long are dropped by the next prune (the
	// bridge refreshes them on start); lookups never return them.
	for channel, l := range file.Conversations {
		if channel != "" && l.Session != "" {
			st.convs[channel] = l
		}
	}
	for old, e := range file.Moved {
		if old != "" && e.To != "" && old != e.To {
			st.moved[old] = e
		}
	}
	for sid, at := range file.Seen {
		if sid != "" {
			st.seen[sid] = at
		}
	}
	// Expired requests stay until the next prune; lookups never return them.
	for _, p := range file.Approvals {
		if p.Channel != "" && p.TS != "" && p.Session != "" && p.Request != "" {
			st.approvals = append(st.approvals, p)
		}
	}
	if extra := len(st.approvals) - maxApprovals; extra > 0 {
		st.approvals = st.approvals[extra:]
	}
	st.users = file.Allowed
	return st, nil
}

// seed puts config users first. A persisted Slack-added entry for the same ID
// is replaced by the config entry, and clashing labels are renumbered.
func (st *state) seed(config []allowedUser) {
	st.mu.Lock()
	defer st.mu.Unlock()
	previous := st.users
	st.users = nil
	seen := map[string]bool{}
	for _, u := range config {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		u.config = true
		st.users = append(st.users, u)
	}
	for _, u := range previous {
		if seen[u.ID] || u.config {
			continue
		}
		seen[u.ID] = true
		u.Label = st.uniqueLabelLocked(u.Label)
		st.users = append(st.users, u)
	}
}

func (st *state) uniqueLabelLocked(base string) string {
	taken := func(label string) bool {
		for _, u := range st.users {
			if u.Label == label {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		if label := base + strconv.Itoa(i); !taken(label) {
			return label
		}
	}
}

func (st *state) user(id string) (allowedUser, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, true
		}
	}
	return allowedUser{}, false
}

func (st *state) labels() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.users))
	for _, u := range st.users {
		out = append(out, u.Label)
	}
	return out
}

// ownerLabels lists the labels of the users seeded from config.
func (st *state) ownerLabels() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, u := range st.users {
		if u.config {
			out = append(out, u.Label)
		}
	}
	return out
}

// mentionIDs maps label to user ID.
func (st *state) mentionIDs() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.Label] = u.ID
	}
	return out
}

// idLabels maps user ID to label.
func (st *state) idLabels() map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]string, len(st.users))
	for _, u := range st.users {
		out[u.ID] = u.Label
	}
	return out
}

// allow adds a user with a frozen label made from rawLabel. It returns the
// existing entry and false when the user is already allowed.
func (st *state) allow(id, rawLabel string) (allowedUser, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if u.ID == id {
			return u, false, nil
		}
	}
	u := allowedUser{ID: id, Label: st.uniqueLabelLocked(sanitizeLabel(rawLabel))}
	st.users = append(st.users, u)
	return u, true, st.saveLocked()
}

func (st *state) remove(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, u := range st.users {
		if u.ID != id {
			continue
		}
		if u.config {
			return errConfigUser
		}
		st.users = append(st.users[:i], st.users[i+1:]...)
		return st.saveLocked()
	}
	return errNotAllowed
}

// thread returns the ts of session sid's home thread.
func (st *state) thread(sid string) (string, bool) {
	ref, ok := st.homeThread(sid)
	return ref.TS, ok
}

// homeThread returns session sid's home thread.
func (st *state) homeThread(sid string) (threadRef, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ref, ok := st.threads[sid]
	return ref, ok
}

func (st *state) session(ts string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sid, ok := st.sessions[ts]
	return sid, ok
}

// setThread links thread ts in channel to a session, so replies in it reach
// the session. When the session has no home thread yet and canBeHome is
// set, ts becomes it, and setThread reports that it did; otherwise the home
// thread stays the one its posts go to.
func (st *state) setThread(sid, channel, ts string, canBeHome bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	first := false
	if _, ok := st.threads[sid]; !ok && canBeHome {
		st.threads[sid] = threadRef{Channel: channel, TS: ts}
		first = true
	}
	if !first && st.sessions[ts] == sid {
		return false
	}
	st.sessions[ts] = sid
	st.dirty = true
	return first
}

// moveThread makes thread ts in channel session sid's home thread, replacing
// any it had. An old thread stays linked, so replies in it still reach sid.
// A non-empty home is recorded as where an owner moved it.
func (st *state) moveThread(sid, channel, ts, home string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.threads[sid] = threadRef{Channel: channel, TS: ts}
	st.sessions[ts] = sid
	if home != "" {
		st.homes[sid] = home
	}
	st.dirty = true
}

// setHomeOwner records whose DM session sid's home was moved to (userID),
// or clears it (empty userID).
func (st *state) setHomeOwner(sid, userID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if userID == "" {
		delete(st.homeDMs, sid)
	} else {
		st.homeDMs[sid] = userID
	}
	st.dirty = true
}

// homeOwner returns the owner whose DM session sid's home was moved to, or
// "" when none was recorded.
func (st *state) homeOwner(sid string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.homeDMs[sid]
}

// home returns where an owner moved session sid's home thread (homeChannel
// or homeDM), or "" when no one did.
func (st *state) home(sid string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.homes[sid]
}

// fillThreadChannels puts channel on home threads loaded from a state file
// that stored a bare ts (all of those are in the main channel), saving when
// it changed any.
func (st *state) fillThreadChannels(channel string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := false
	for sid, ref := range st.threads {
		if ref.Channel == "" {
			ref.Channel = channel
			st.threads[sid] = ref
			changed = true
		}
	}
	if !changed {
		return
	}
	st.dirty = true
}

// record remembers delivered message r (stamped now): where it came from
// and the session it went to. Expired entries are dropped, and the oldest
// when there are more than maxReplies. It returns r's receipt reaction:
// r.Receipt, or a later one that arrived before the record (early).
func (st *state) record(r replyRecord) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e, ok := st.early[r.ID]; ok {
		delete(st.early, r.ID)
		if receiptRank(e) > receiptRank(r.Receipt) {
			r.Receipt = e
		}
	}
	now := st.now()
	cutoff := now.Add(-replyTTL)
	kept := st.replies[:0]
	for _, r := range st.replies {
		if r.At.After(cutoff) {
			kept = append(kept, r)
		}
	}
	r.At = now
	kept = append(kept, r)
	if extra := len(kept) - maxReplies; extra > 0 {
		kept = kept[:copy(kept, kept[extra:])]
	}
	st.replies = kept
	st.dirty = true
	return r.Receipt
}

// replyLocked finds the unexpired entry for msgID. The caller holds st.mu.
func (st *state) replyLocked(msgID string) (replyRecord, bool) {
	i, ok := st.replyIndexLocked(msgID)
	if !ok {
		return replyRecord{}, false
	}
	return st.replies[i], true
}

// replyIndexLocked finds the index of the unexpired entry for msgID in
// st.replies. The caller holds st.mu.
func (st *state) replyIndexLocked(msgID string) (int, bool) {
	if msgID == "" {
		return 0, false
	}
	cutoff := st.now().Add(-replyTTL)
	for i := len(st.replies) - 1; i >= 0; i-- {
		if r := st.replies[i]; r.ID == msgID {
			return i, r.At.After(cutoff)
		}
	}
	return 0, false
}

// knownLocked reports whether msgID has a record, expired or not. The caller
// holds st.mu.
func (st *state) knownLocked(msgID string) bool {
	for i := len(st.replies) - 1; i >= 0; i-- {
		if st.replies[i].ID == msgID {
			return true
		}
	}
	return false
}

// noteEarlyLocked keeps a receipt for msgID until record sees it, evicting
// the oldest past maxEarlyReceipts. The caller holds st.mu.
func (st *state) noteEarlyLocked(msgID, reaction string) {
	if prev, ok := st.early[msgID]; ok {
		if receiptRank(reaction) > receiptRank(prev) {
			st.early[msgID] = reaction
		}
		return
	}
	st.early[msgID] = reaction
	st.earlyRing = append(st.earlyRing, msgID)
	if len(st.earlyRing) > maxEarlyReceipts {
		delete(st.early, st.earlyRing[0])
		st.earlyRing = st.earlyRing[1:]
	}
}

// replyTarget returns the record of msgID when it was delivered to sid (or
// to a session sid took over, or that took sid over; see moveSession) and
// hasn't expired: the conversation and thread it came from. The thread is
// empty for a top-level DM.
func (st *state) replyTarget(msgID, sid string) (replyRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.replyLocked(msgID)
	if !ok || !st.sameSessionLocked(r.Session, sid) {
		return replyRecord{}, false
	}
	return r, true
}

// replyOwner returns the session an unexpired msgID was delivered to.
func (st *state) replyOwner(msgID string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.replyLocked(msgID)
	return r.Session, ok
}

// linkDM ties the top-level message ts in DM channel to session sid; agent
// marks the session's own post. Expired links are dropped, and the oldest
// when there are more than maxDMLinks.
func (st *state) linkDM(channel, ts, sid string, agent bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	cutoff := now.Add(-replyTTL)
	kept := st.dmLinks[:0]
	for _, l := range st.dmLinks {
		if l.At.After(cutoff) {
			kept = append(kept, l)
		}
	}
	kept = append(kept, dmLink{Channel: channel, TS: ts, Session: sid, Agent: agent, At: now})
	if extra := len(kept) - maxDMLinks; extra > 0 {
		kept = kept[:copy(kept, kept[extra:])]
	}
	st.dmLinks = kept
	st.dirty = true
}

// dmSession returns the session an unexpired top-level DM message ts in
// channel is tied to.
func (st *state) dmSession(channel, ts string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-replyTTL)
	for i := len(st.dmLinks) - 1; i >= 0; i-- {
		if l := st.dmLinks[i]; l.Channel == channel && l.TS == ts {
			return l.Session, l.At.After(cutoff)
		}
	}
	return "", false
}

// dmHeaded reports whether the newest unexpired top-level post by an agent
// in DM channel (its own post, its header, or the header of its home thread
// there) is session sid's, so a post by sid there needs no header line. A
// post by another agent in between means sid's next one carries its header
// again.
func (st *state) dmHeaded(channel, sid string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-replyTTL)
	for i := len(st.dmLinks) - 1; i >= 0; i-- {
		if l := st.dmLinks[i]; l.Agent && l.Channel == channel && l.At.After(cutoff) {
			return l.Session == sid
		}
	}
	return false
}

// setDMLast makes sid the agent userID's plain DMs go to.
func (st *state) setDMLast(userID, sid string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dmLasts[userID] = dmLastEntry{Session: sid, At: st.now()}
	st.dirty = true
}

// dmLast returns the agent userID last talked to in their DM, unless that
// was more than dmLastTTL ago.
func (st *state) dmLast(userID string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.dmLasts[userID]
	if !ok || !e.At.After(st.now().Add(-dmLastTTL)) {
		return "", false
	}
	return e.Session, true
}

// userByLabel finds an allowed user by label, compared case-insensitively.
func (st *state) userByLabel(label string) (allowedUser, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range st.users {
		if strings.EqualFold(u.Label, label) {
			return u, true
		}
	}
	return allowedUser{}, false
}

// threadSession returns the session a thread in a conversation other than
// the main channel goes to by its reply records: the one its first message
// was delivered to, else the one the newest unexpired message in the thread
// was. Records that exist only because of a conversation link don't count,
// so after an unlink only tags (and threads under them) reach agents there.
func (st *state) threadSession(channel, threadTS string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-replyTTL)
	newest := ""
	for i := len(st.replies) - 1; i >= 0; i-- {
		r := st.replies[i]
		if r.Channel != channel || r.Link || !r.At.After(cutoff) {
			continue
		}
		if r.TS == threadTS {
			return r.Session, true
		}
		if newest == "" && r.ThreadTS == threadTS {
			newest = r.Session
		}
	}
	return newest, newest != ""
}

// linkConversation links channel (of kind) to sid, as owner by did, with
// members known to be in it (added to those a live link there knew),
// replacing any link it had. It returns the session channel was linked to
// before (empty for none) and the save error.
func (st *state) linkConversation(channel, sid, by, kind string, members []string) (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := ""
	var known []string
	if l, ok := st.liveConvLocked(channel); ok {
		prev, known = l.Session, l.Members
	}
	for _, id := range members {
		if !slices.Contains(known, id) && len(known) < maxLinkMembers {
			known = append(known, id)
		}
	}
	now := st.now()
	st.convs[channel] = convLink{Session: sid, By: by, At: now, Seen: now, Kind: kind, Members: slices.Clone(known)}
	return prev, st.saveLocked()
}

// unlinkConversation removes channel's link. For a DM, the person whose DM
// it is no longer has their plain messages sent to the unlinked agent: a
// dm_last pointing at it is cleared. It returns the link, whether there was
// a live one, and the save error.
func (st *state) unlinkConversation(channel string) (convLink, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.liveConvLocked(channel)
	raw, present := st.convs[channel]
	if !present {
		return convLink{}, false, nil
	}
	delete(st.convs, channel)
	if raw.Kind == kindDM || strings.HasPrefix(channel, "D") {
		for _, member := range raw.Members {
			if e, has := st.dmLasts[member]; has && e.Session == raw.Session {
				delete(st.dmLasts, member)
			}
		}
	}
	return l, ok, st.saveLocked()
}

// noteMember records userID as in channel's live link, saving when it is
// new there.
func (st *state) noteMember(channel, userID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.liveConvLocked(channel)
	if !ok || slices.Contains(l.Members, userID) || len(l.Members) >= maxLinkMembers {
		return
	}
	l.Members = append(slices.Clone(l.Members), userID)
	st.convs[channel] = l
	st.dirty = true
}

// conversations lists the live links, oldest first.
func (st *state) conversations() []linkedConv {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []linkedConv
	for channel := range st.convs {
		if l, ok := st.liveConvLocked(channel); ok {
			out = append(out, linkedConv{Channel: channel, convLink: l})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Channel < out[j].Channel
	})
	return out
}

// conversation returns channel's link, unless its session has been absent
// for more than linkAbsentTTL by its recorded Seen. Callers that decide on
// routing use Bridge.conversationLink, which refreshes Seen from the bus
// first.
func (st *state) conversation(channel string) (convLink, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.liveConvLocked(channel)
}

// convSession returns the session channel is linked to, live or not.
func (st *state) convSession(channel string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.convs[channel]
	return l.Session, ok
}

// refreshConversation moves channel's Seen up to seen when known (the bus
// knows the session) and it is later, then returns the link if it is live.
// A link that is not live is dropped. Only sid's link is refreshed, so a
// relink since the caller asked the bus isn't touched.
func (st *state) refreshConversation(channel, sid string, seen time.Time, known bool) (convLink, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.convs[channel]
	if !ok {
		return convLink{}, false
	}
	if known && l.Session == sid && seen.After(l.Seen) {
		l.Seen = seen
		st.convs[channel] = l
	}
	if live, isLive := st.liveConvLocked(channel); isLive {
		return live, true
	}
	delete(st.convs, channel)
	st.dirty = true
	return convLink{}, false
}

func (st *state) liveConvLocked(channel string) (convLink, bool) {
	l, ok := st.convs[channel]
	if !ok || !l.Seen.After(st.now().Add(-linkAbsentTTL)) {
		return convLink{}, false
	}
	return l, true
}

// conversationSessions lists the sessions conversations are linked to.
func (st *state) conversationSessions() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, l := range st.convs {
		out = append(out, l.Session)
	}
	return out
}

// touchConversations moves each link's Seen up to when its session was
// last on the bus (seen maps session id to that time), then drops the links
// of sessions absent for more than linkAbsentTTL, saving when it dropped
// any. Seen alone changes in memory and is saved with the next change.
func (st *state) touchConversations(seen map[string]time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for channel, l := range st.convs {
		if at, ok := seen[l.Session]; ok && at.After(l.Seen) {
			l.Seen = at
			st.convs[channel] = l
		}
	}
	if st.pruneConvsLocked() {
		st.dirty = true
	}
}

// pruneConvsLocked drops links whose session has been absent for more than
// linkAbsentTTL and reports whether it dropped any. The caller holds st.mu.
func (st *state) pruneConvsLocked() bool {
	pruned := false
	for channel := range st.convs {
		if _, ok := st.liveConvLocked(channel); !ok {
			delete(st.convs, channel)
			pruned = true
		}
	}
	return pruned
}

// saveLocked writes the whole state to disk now and clears dirty. The
// caller holds st.mu.
func (st *state) saveLocked() error {
	if st.path == "" {
		return nil
	}
	// Links are pruned only after their Seen was refreshed from the bus
	// (touchConversations, refreshConversation), never here: the bus can't
	// be asked under st.mu.
	file := stateFile{Threads: st.threads, Links: st.sessions, Replies: st.replies, DMLinks: st.dmLinks}
	if len(st.homes) > 0 {
		file.Homes = st.homes
	}
	if len(st.moved) > 0 {
		file.Moved = st.moved
	}
	if len(st.homeDMs) > 0 {
		file.HomeOwners = st.homeDMs
	}
	if len(st.seen) > 0 {
		file.Seen = st.seen
	}
	if len(st.convs) > 0 {
		file.Conversations = st.convs
	}
	if len(st.approvals) > 0 {
		file.Approvals = st.approvals
	}
	cutoff := st.now().Add(-dmLastTTL)
	for user, e := range st.dmLasts {
		if e.At.After(cutoff) {
			if file.DMLast == nil {
				file.DMLast = map[string]dmLastEntry{}
			}
			file.DMLast[user] = e
		}
	}
	for _, u := range st.users {
		if !u.config {
			file.Allowed = append(file.Allowed, u)
		}
	}
	data, errJSON := json.Marshal(file)
	if errJSON != nil {
		return errJSON
	}
	if errMkdir := os.MkdirAll(filepath.Dir(st.path), 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp := st.path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	if errRename := os.Rename(tmp, st.path); errRename != nil {
		return errRename
	}
	st.dirty = false
	return nil
}
