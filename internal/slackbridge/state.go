package slackbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxReplies caps the remembered deliveries; the oldest go first.
	maxReplies = 1000
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
	// Link marks a message that reached Session only because Channel was
	// linked to it (a guest's message, or the link notice). An answer goes
	// there only while Channel is still linked to Session.
	Link bool `json:"link,omitempty"`
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
}

// state holds session threads, delivered messages, DM routing and the
// allowlist. Every change is written through to disk; changes are rare
// (human-paced).
type state struct {
	path     string
	now      func() time.Time
	mu       sync.Mutex
	threads  map[string]threadRef   // session id -> its home thread (agents post there)
	homes    map[string]string      // session id -> where an owner moved its home thread
	sessions map[string]string      // thread ts -> session id, for every linked thread
	replies  []replyRecord          // delivered messages, oldest first, at most maxReplies
	dmLinks  []dmLink               // top-level DM messages, oldest first, at most maxDMLinks
	dmLasts  map[string]dmLastEntry // user ID -> the agent they last talked to in their DM
	convs    map[string]convLink    // channel ID -> the session the conversation is linked to
	users    []allowedUser          // config users first
	// early holds receipts for message ids not recorded yet (a waiter can
	// claim a message before deliver records it); record applies them. Not
	// persisted; earlyRing evicts the oldest past maxEarlyReceipts.
	early     map[string]string
	earlyRing []string
}

// loadState reads path; a missing file is an empty state. On a corrupt file it
// returns an empty state and the error.
func loadState(path string) (*state, error) {
	st := &state{path: path, now: time.Now, threads: map[string]threadRef{}, homes: map[string]string{}, sessions: map[string]string{}, dmLasts: map[string]dmLastEntry{}, convs: map[string]convLink{}, early: map[string]string{}}
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
	// Expired entries stay until the next recordReply drops them; lookups
	// never return them.
	// A record of a top-level DM has no thread, but always a channel.
	for _, r := range file.Replies {
		if r.ID != "" && r.Session != "" && (r.ThreadTS != "" || r.Channel != "") {
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
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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

// receiptChange is a receipt reaction to add to message ts in channel, and
// the one it replaces (empty for none).
type receiptChange struct {
	channel, ts, add, remove string
}

// advanceReceipts moves each recorded message in ids to receipt reaction
// when that is further along than its current one, and returns the
// reactions to change. A receipt never moves back. An id with no record yet
// is kept in early for record; an expired one, or one recorded before
// receipts (no TS), is ignored.
func (st *state) advanceReceipts(ids []string, reaction string) []receiptChange {
	st.mu.Lock()
	defer st.mu.Unlock()
	rank := receiptRank(reaction)
	var out []receiptChange
	for _, id := range ids {
		if id == "" {
			continue
		}
		i, ok := st.replyIndexLocked(id)
		if !ok {
			if !st.knownLocked(id) {
				st.noteEarlyLocked(id, reaction)
			}
			continue
		}
		r := &st.replies[i]
		if r.TS == "" || rank <= receiptRank(r.Receipt) {
			continue
		}
		out = append(out, receiptChange{channel: r.Channel, ts: r.TS, add: reaction, remove: r.Receipt})
		r.Receipt = reaction
	}
	if len(out) > 0 {
		if errSave := st.saveLocked(); errSave != nil {
			logSaveError(errSave)
		}
	}
	return out
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

// replyTarget returns the record of msgID when it was delivered to sid and
// hasn't expired: the conversation and thread it came from. The thread is
// empty for a top-level DM.
func (st *state) replyTarget(msgID, sid string) (replyRecord, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.replyLocked(msgID)
	if !ok || r.Session != sid {
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
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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

// dmHeaded reports whether session sid has an unexpired post of its own in
// DM channel, so its header line went out there.
func (st *state) dmHeaded(channel, sid string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-replyTTL)
	for _, l := range st.dmLinks {
		if l.Agent && l.Channel == channel && l.Session == sid && l.At.After(cutoff) {
			return true
		}
	}
	return false
}

// setDMLast makes sid the agent userID's plain DMs go to.
func (st *state) setDMLast(userID, sid string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dmLasts[userID] = dmLastEntry{Session: sid, At: st.now()}
	if errSave := st.saveLocked(); errSave != nil {
		logSaveError(errSave)
	}
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

// linkConversation links channel to sid, as owner by did, replacing any
// link it had. It returns the session channel was linked to before (empty
// for none) and the save error.
func (st *state) linkConversation(channel, sid, by string) (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	prev := ""
	if l, ok := st.liveConvLocked(channel); ok {
		prev = l.Session
	}
	now := st.now()
	st.convs[channel] = convLink{Session: sid, By: by, At: now, Seen: now}
	return prev, st.saveLocked()
}

// unlinkConversation removes channel's link. It returns the session it was
// linked to, whether there was a live link, and the save error.
func (st *state) unlinkConversation(channel string) (string, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	l, ok := st.liveConvLocked(channel)
	if _, present := st.convs[channel]; !present {
		return "", false, nil
	}
	delete(st.convs, channel)
	return l.Session, ok, st.saveLocked()
}

// conversation returns channel's link, unless its session has been absent
// for more than linkAbsentTTL.
func (st *state) conversation(channel string) (convLink, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.liveConvLocked(channel)
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
		if errSave := st.saveLocked(); errSave != nil {
			logSaveError(errSave)
		}
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

func (st *state) saveLocked() error {
	if st.path == "" {
		return nil
	}
	st.pruneConvsLocked()
	file := stateFile{Threads: st.threads, Links: st.sessions, Replies: st.replies, DMLinks: st.dmLinks}
	if len(st.homes) > 0 {
		file.Homes = st.homes
	}
	if len(st.convs) > 0 {
		file.Conversations = st.convs
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
	return os.Rename(tmp, st.path)
}
