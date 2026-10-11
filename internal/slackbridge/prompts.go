package slackbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Prompts: blocking Claude Code prompts the agentbus mod opens through
// /v1/agentbus/prompt. The bridge posts each to the first owner's DM as
// Block Kit buttons, takes an owner's click or modal as the answer, and keeps
// the message in step with how the prompt ends. State lives in memory only.
// Every Socket Mode handler here touches memory only; Slack calls run in
// jobs or in the caller's goroutine.

const (
	promptModalID      = "prompt_modal"
	promptActionPrefix = "prompt:"

	// maxPromptsPerSession and maxPrompts cap the prompts open at once.
	maxPromptsPerSession = 20
	maxPrompts           = 200
	// promptTTL is how long a prompt stays open before it closes by itself.
	promptTTL = 24 * time.Hour
	// promptRetain is how long after a prompt closed a wait that already holds
	// its id still gets the answer. A new open with the same key never gets a
	// closed prompt back: it makes a new one.
	promptRetain = 10 * time.Minute
	// promptForget is how long a closed prompt's record is kept at all.
	promptForget = time.Hour
	// maxClosedPrompts caps the closed records kept; the oldest go first.
	maxClosedPrompts = 1000
	// maxStaleEdits caps the stale-message edits waiting for the redraw loop.
	maxStaleEdits = 50
	// promptWaitMax is one long-poll of /prompt/wait.
	promptWaitMax = 25 * time.Second
	// promptSweepEvery is how often the bridge sweeps prompts.
	promptSweepEvery = 5 * time.Second
	// maxRedrawFails is how many failed edits in a row a prompt's message
	// gets before the bridge gives up on it.
	maxRedrawFails = 10
	// maxReasonRunes caps a deny or keep-planning reason and a text answer.
	maxReasonRunes = 3000

	notOwnerAnswer = "Only an owner can answer this."
	alreadyAnswer  = "Already answered."
	noLongerOpen   = "This prompt is no longer open."
	staleAnswer    = "This prompt is from before a restart and can't be answered here; answer in the terminal."
	staleStatus    = "From before a restart; answer in the terminal."

	// outcomeExpired is the outcome of a prompt that outlived promptTTL.
	outcomeExpired = "expired"
	// outcomeFailed is the outcome of a prompt Slack would not take.
	outcomeFailed = "failed"
)

type promptState int

const (
	promptOpenState promptState = iota
	promptAnsweredState
	promptClosedState
)

// prompt is one open or recently closed prompt.
type prompt struct {
	id, session, key string
	// req is the request as the mod sent it; answers use its labels and ids.
	req      agentbus.PromptRequest
	openedAt time.Time

	state      promptState
	answer     *agentbus.PromptAnswer
	answeredAt time.Time
	outcome    string
	// note is the mod's note on the resolve that closed the prompt ("terminal"
	// when the call was settled at the keyboard).
	note     string
	closedAt time.Time

	// channel and ts locate the posted message; empty until it is posted.
	channel, ts string
	// posted is closed when the post finished (postFailed says how).
	posted     chan struct{}
	postFailed bool
	// changed is closed (and replaced) on every state change, to wake waiters.
	changed chan struct{}

	// dirty: the message must be redrawn (the prompt is in b.promptDirty).
	// fails and retryAt pace the retries of a failed edit.
	dirty   bool
	fails   int
	retryAt time.Time
}

// staleEdit is an edit of a message of a prompt this proxy no longer knows
// (from before a restart): its buttons come off.
type staleEdit struct{ channel, ts, text, blocks string }

// promptThread is the top-level DM message a session's prompts are posted
// under, when its home thread is not in the owner's DM.
type promptThread struct{ channel, ts string }

// promptSnap is what a render needs of a prompt, copied under the lock.
type promptSnap struct {
	id      string
	req     agentbus.PromptRequest
	state   promptState
	answer  *agentbus.PromptAnswer
	outcome string
	note    string
}

func newPromptID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to the clock so an
		// id is never empty.
		return fmt.Sprintf("p_%032x", time.Now().UnixNano())
	}
	return "p_" + hex.EncodeToString(buf[:])
}

func (b *Bridge) workCtx() context.Context {
	if b.runCtx != nil {
		return b.runCtx
	}
	return context.Background()
}

func (p *prompt) snap() promptSnap {
	s := promptSnap{id: p.id, req: p.req, state: p.state, outcome: p.outcome, note: p.note}
	if p.answer != nil {
		a := *p.answer
		s.answer = &a
	}
	return s
}

// wakeLocked wakes everything waiting on p. The caller holds b.promptMu.
func (p *prompt) wakeLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// PromptsAvailable reports whether prompts can be offered now: the socket is
// connected, the first owner (whose DM prompts go to) is still an owner, and
// their DM can be opened (the opened channel is cached, so only a failure
// costs a Slack call). It implements agentbus.PromptBridge.
func (b *Bridge) PromptsAvailable() bool {
	if !b.connected.Load() || !b.ownerReady() {
		return false
	}
	if _, err := b.dmChannel(b.workCtx(), b.ownerID); err != nil {
		log.Warnf("slack: prompts unavailable, can't open the owner's DM: %v", err)
		return false
	}
	return true
}

func (b *Bridge) ownerReady() bool {
	return b.ownerID != "" && b.IsOwner(b.ownerID)
}

// initPromptsLocked makes the prompt maps. The caller holds b.promptMu.
func (b *Bridge) initPromptsLocked() {
	if b.promptByID == nil {
		b.promptByID = map[string]*prompt{}
		b.promptByKey = map[string]*prompt{}
		b.promptThreads = map[string]promptThread{}
		b.promptDirty = map[string]*prompt{}
	}
}

func promptKeyOf(p *prompt) string { return p.session + "\x00" + p.key }

// forgetPromptLocked drops p's record. The caller holds b.promptMu.
func (b *Bridge) forgetPromptLocked(p *prompt) {
	delete(b.promptByID, p.id)
	delete(b.promptDirty, p.id)
	if b.promptByKey[promptKeyOf(p)] == p {
		delete(b.promptByKey, promptKeyOf(p))
	}
}

// expireDueLocked closes p when it outlived promptTTL. The caller holds
// b.promptMu.
func (b *Bridge) expireDueLocked(p *prompt, now time.Time) {
	if p.state != promptClosedState && now.Sub(p.openedAt) >= promptTTL {
		b.closePromptLocked(p, outcomeExpired, "", now)
	}
}

// expirePromptsLocked closes prompts older than promptTTL and forgets closed
// ones after promptForget. Only the sweep runs it. The caller holds
// b.promptMu.
func (b *Bridge) expirePromptsLocked(now time.Time) {
	for _, p := range b.promptByID {
		b.expireDueLocked(p, now)
	}
	for len(b.closedPrompts) > 0 && now.Sub(b.closedPrompts[0].closedAt) >= promptForget {
		b.dropOldestClosedLocked()
	}
}

// dropOldestClosedLocked forgets the oldest closed record. The caller holds
// b.promptMu.
func (b *Bridge) dropOldestClosedLocked() {
	b.forgetPromptLocked(b.closedPrompts[0])
	b.closedPrompts[0] = nil
	b.closedPrompts = b.closedPrompts[1:]
}

// closePromptLocked closes an open or answered prompt, wakes its waiters and
// marks its message for redraw. A closed prompt is never reused by a new
// open, only found by its id. The caller holds b.promptMu.
func (b *Bridge) closePromptLocked(p *prompt, outcome, note string, now time.Time) {
	if p.state == promptClosedState {
		return
	}
	p.state, p.outcome, p.note, p.closedAt = promptClosedState, outcome, note, now
	if b.promptByKey[promptKeyOf(p)] == p {
		delete(b.promptByKey, promptKeyOf(p))
	}
	p.wakeLocked()
	b.closedPrompts = append(b.closedPrompts, p)
	for len(b.closedPrompts) > maxClosedPrompts {
		b.dropOldestClosedLocked()
	}
	b.markPromptDirtyLocked(p)
}

// scrubPromptLocked drops what a closed prompt's final message was drawn
// from, keeping a short title for the logs. The caller holds b.promptMu.
func scrubPromptLocked(p *prompt) {
	p.req.Detail, p.req.Questions = "", nil
	p.req.Title = firstRunes(p.req.Title, maxHeaderRunes)
	p.req.Agent = firstRunes(p.req.Agent, 100)
}

// markPromptDirtyLocked records that p's message must be redrawn and wakes
// the redraw loop. A prompt not posted yet is drawn by its post. The caller
// holds b.promptMu.
func (b *Bridge) markPromptDirtyLocked(p *prompt) {
	p.dirty = true
	if p.ts == "" {
		return
	}
	b.promptDirty[p.id] = p
	b.wakeRedraw()
}

// wakeRedraw nudges the redraw loop; a nudge already pending is enough.
func (b *Bridge) wakeRedraw() {
	select {
	case b.redrawWake <- struct{}{}:
	default:
	}
}

// reservePrompt returns the live (open, or answered but not yet resolved)
// prompt for req's (session, key), or reserves a new one. isNew says the
// caller must post it. A closed prompt is never returned: its key opens a
// new prompt.
func (b *Bridge) reservePrompt(req agentbus.PromptRequest) (p *prompt, isNew bool, err error) {
	now := b.state.now()
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	b.initPromptsLocked()
	k := req.Session + "\x00" + req.Key
	if old := b.promptByKey[k]; old != nil {
		b.expireDueLocked(old, now)
		// An answer whose session never resolved the prompt (its resolve was
		// lost) is not handed to a new call past promptRetain: that would
		// replay an old answer. The old record stays findable by id.
		stale := old.state == promptAnsweredState && now.Sub(old.answeredAt) > promptRetain
		if old.state != promptClosedState && !stale {
			return old, false, nil
		}
	}
	total, mine := 0, 0
	for _, q := range b.promptByID {
		if q.state == promptClosedState {
			continue
		}
		total++
		if q.session == req.Session {
			mine++
		}
	}
	if total >= maxPrompts || mine >= maxPromptsPerSession {
		return nil, false, agentbus.ErrPromptsBusy
	}
	p = &prompt{
		id: newPromptID(), session: req.Session, key: req.Key, req: req, openedAt: now,
		posted: make(chan struct{}), changed: make(chan struct{}),
	}
	b.promptByID[p.id] = p
	b.promptByKey[k] = p
	return p, true, nil
}

// OpenPrompt posts req to the first owner's DM and returns the prompt's id.
// A repeat for the same (session, key) while the prompt is open, or answered
// but not resolved, returns the same id and posts nothing; a concurrent open
// waits for the first one's post. Once the prompt closed, the key opens a new
// prompt. It implements agentbus.PromptBridge and runs in the caller's
// goroutine.
func (b *Bridge) OpenPrompt(ctx context.Context, o agentbus.Outbound, req agentbus.PromptRequest) (string, error) {
	if !b.ownerReady() {
		return "", agentbus.ErrPromptsOff
	}
	req.Session = o.SessionID
	p, isNew, err := b.reservePrompt(req)
	if err != nil {
		return "", err
	}
	if isNew {
		b.postPrompt(o, p)
	} else {
		select {
		case <-p.posted:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	b.promptMu.Lock()
	failed := p.postFailed
	b.promptMu.Unlock()
	if failed {
		return "", agentbus.ErrPromptPost
	}
	return p.id, nil
}

// postPrompt posts p. It retries once, and only when Slack surely did not
// take the post; after an ambiguous failure a retry could double-post, so the
// prompt closes instead. It uses the bridge's own context, not the request's,
// so a client that gave up can't abort a post Slack already made.
func (b *Bridge) postPrompt(o agentbus.Outbound, p *prompt) {
	ctx := b.workCtx()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && !b.sleep(ctx, b.retryDelay) {
			break
		}
		var retry bool
		if retry, err = b.tryPostPrompt(ctx, o, p); err == nil {
			break
		}
		log.Warnf("slack: prompt post for %s failed: %v", o.Address, err)
		if !retry {
			break
		}
	}
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	if err != nil {
		p.postFailed = true
		b.closePromptLocked(p, outcomeFailed, "", b.state.now())
		scrubPromptLocked(p)
	} else if p.dirty || p.state != promptOpenState {
		// Answered or closed while it was being posted: draw the latest.
		b.markPromptDirtyLocked(p)
	}
	close(p.posted)
}

// postNotAccepted reports whether err from a Slack call means Slack did not
// take the request: it answered with an error, or the connection failed
// before the request was sent. Any other failure may have posted.
func postNotAccepted(err error) bool {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// tryPostPrompt makes one attempt to post p. retry says a second attempt is
// safe when this one failed.
func (b *Bridge) tryPostPrompt(ctx context.Context, o agentbus.Outbound, p *prompt) (retry bool, err error) {
	channel, thread, err := b.promptTarget(ctx, o)
	if err != nil {
		// Nothing of the prompt itself was sent yet.
		return true, err
	}
	b.promptMu.Lock()
	snap := p.snap()
	b.promptMu.Unlock()
	text, blocks := b.renderPrompt(snap)
	ts, err := b.api.postBlocks(ctx, b.cfg.BotToken, channel, text, blocks, thread)
	if err != nil {
		return postNotAccepted(err), err
	}
	b.promptMu.Lock()
	p.channel, p.ts = channel, ts
	b.promptMu.Unlock()
	return false, nil
}

// promptTarget picks where session o's prompt goes: the first owner's DM,
// in the session's home thread when that lives there, else in a thread under
// a header post the bridge makes once per session. The header is its own
// post, made before the prompt, and routes thread replies to the session as
// any session header does.
func (b *Bridge) promptTarget(ctx context.Context, o agentbus.Outbound) (channel, thread string, err error) {
	channel, err = b.dmChannel(ctx, b.ownerID)
	if err != nil {
		return "", "", err
	}
	if t, ok := b.ownThread(o.SessionID); ok && t.channel == channel {
		return channel, t.threadTS, nil
	}
	unlock, err := b.lockOpening(ctx, o.SessionID)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	if t, ok := b.ownThread(o.SessionID); ok && t.channel == channel {
		return channel, t.threadTS, nil
	}
	b.promptMu.Lock()
	b.initPromptsLocked()
	pt, ok := b.promptThreads[o.SessionID]
	b.promptMu.Unlock()
	if ok && pt.channel == channel {
		return channel, pt.ts, nil
	}
	ts, err := b.api.postMessage(ctx, b.cfg.BotToken, channel, sessionHeader(o), "")
	if err != nil {
		return "", "", err
	}
	b.state.linkDM(channel, ts, o.SessionID, true)
	b.promptMu.Lock()
	b.promptThreads[o.SessionID] = promptThread{channel: channel, ts: ts}
	b.promptMu.Unlock()
	return channel, ts, nil
}

// WaitPrompt long-polls for session's prompt id. It implements
// agentbus.PromptBridge.
func (b *Bridge) WaitPrompt(ctx context.Context, session, id string) agentbus.PromptWait {
	limit := b.promptWaitFor
	if limit <= 0 {
		limit = promptWaitMax
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		now := b.state.now()
		b.promptMu.Lock()
		b.initPromptsLocked()
		p := b.promptByID[id]
		if p == nil || p.session != session {
			b.promptMu.Unlock()
			return agentbus.PromptWait{State: agentbus.PromptGone}
		}
		b.expireDueLocked(p, now)
		switch {
		case p.state == promptClosedState && now.Sub(p.closedAt) > promptRetain:
			b.promptMu.Unlock()
			return agentbus.PromptWait{State: agentbus.PromptClosed}
		case p.answer != nil:
			a := *p.answer
			b.promptMu.Unlock()
			return agentbus.PromptWait{State: agentbus.PromptAnswered, Answer: &a}
		case p.state == promptClosedState:
			b.promptMu.Unlock()
			return agentbus.PromptWait{State: agentbus.PromptClosed}
		}
		changed := p.changed
		b.promptMu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return agentbus.PromptWait{State: agentbus.PromptOpen}
		case <-ctx.Done():
			return agentbus.PromptWait{State: agentbus.PromptOpen}
		}
	}
}

// ResolvePrompt closes session's prompt id with outcome (the mod's account of
// how the call ended) and note. The first close wins; a repeat is fine. It
// implements agentbus.PromptBridge.
func (b *Bridge) ResolvePrompt(session, id, outcome, note string) error {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	b.initPromptsLocked()
	p := b.promptByID[id]
	if p == nil || p.session != session {
		return agentbus.ErrPromptUnknown
	}
	b.closePromptLocked(p, outcome, strings.TrimSpace(note), b.state.now())
	return nil
}

// SessionEnded closes every prompt of a session that said bye and forgets its
// prompt thread. It implements agentbus.PromptBridge.
func (b *Bridge) SessionEnded(session string) {
	b.closeSessionPrompts(session)
	b.forgetPromptThreads(session)
}

// closeSessionPrompts closes the open prompts of sessions as ended.
func (b *Bridge) closeSessionPrompts(sessions ...string) {
	now := b.state.now()
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	for _, p := range b.promptByID {
		for _, sid := range sessions {
			if p.session == sid {
				b.closePromptLocked(p, agentbus.OutcomeEnded, "", now)
			}
		}
	}
}

// forgetPromptThreads drops the prompt threads of sessions that are gone.
func (b *Bridge) forgetPromptThreads(sessions ...string) {
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	for _, sid := range sessions {
		delete(b.promptThreads, sid)
	}
}

// movePrompts closes the prompts of session oldID after a hand-off and
// carries its prompt thread over to newID.
func (b *Bridge) movePrompts(oldID, newID string) {
	b.closeSessionPrompts(oldID)
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	b.initPromptsLocked()
	if pt, ok := b.promptThreads[oldID]; ok {
		if _, has := b.promptThreads[newID]; !has {
			b.promptThreads[newID] = pt
		}
		delete(b.promptThreads, oldID)
	}
}

// sweepPrompts is the prompts' housekeeping: it closes prompts past their
// TTL and those of sessions that went offline (their lease lapsed), forgets
// old closed ones, and wakes the redraw loop for edits that are due again.
// It is the only place that scans every prompt for expiry.
func (b *Bridge) sweepPrompts() {
	var sessions []string
	seen := map[string]bool{}
	b.promptMu.Lock()
	for _, p := range b.promptByID {
		if p.state != promptClosedState && !seen[p.session] {
			seen[p.session] = true
			sessions = append(sessions, p.session)
		}
	}
	b.promptMu.Unlock()
	var gone []string
	if b.bus != nil {
		for _, sid := range sessions {
			if st := b.bus.SessionStatus(sid); st == "" || st == agentbus.StatusOffline {
				gone = append(gone, sid)
			}
		}
	}
	if len(gone) > 0 {
		b.closeSessionPrompts(gone...)
		b.forgetPromptThreads(gone...)
	}
	now := b.state.now()
	b.promptMu.Lock()
	b.initPromptsLocked()
	b.expirePromptsLocked(now)
	pending := len(b.promptDirty) > 0 || len(b.staleEdits) > 0
	b.promptMu.Unlock()
	if pending {
		b.wakeRedraw()
	}
}

// runRedraws is the one goroutine that edits prompt messages, so edits never
// share a queue with owner commands or answers. It runs until ctx ends.
func (b *Bridge) runRedraws(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.redrawWake:
			b.drainRedraws(ctx)
		}
	}
}

// drainRedraws draws every stale message and every dirty prompt whose retry
// time has come. A prompt that fails stays dirty for the sweep to wake again.
func (b *Bridge) drainRedraws(ctx context.Context) {
	b.promptMu.Lock()
	stale := b.staleEdits
	b.staleEdits = nil
	b.promptMu.Unlock()
	for _, e := range stale {
		if ctx.Err() != nil {
			return
		}
		if err := b.api.updateMessage(ctx, b.cfg.BotToken, e.channel, e.ts, e.text, e.blocks); err != nil {
			log.Warnf("slack: could not close a stale prompt message: %v", err)
		}
	}
	now := b.state.now()
	b.promptMu.Lock()
	var due []*prompt
	for _, p := range b.promptDirty {
		if !now.Before(p.retryAt) {
			due = append(due, p)
		}
	}
	b.promptMu.Unlock()
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		b.redrawPrompt(ctx, p)
	}
}

// redrawPrompt redraws p's message. It renders the state at the moment it
// runs and loops while the prompt changes under it, so the last state always
// lands and a stale edit can't bring the buttons back. Once a closed
// prompt's final state is drawn (or given up on), its large fields go.
func (b *Bridge) redrawPrompt(ctx context.Context, p *prompt) {
	for {
		b.promptMu.Lock()
		if !p.dirty || p.ts == "" {
			delete(b.promptDirty, p.id)
			b.promptMu.Unlock()
			return
		}
		p.dirty = false
		snap, channel, ts := p.snap(), p.channel, p.ts
		b.promptMu.Unlock()
		text, blocks := b.renderPrompt(snap)
		err := b.api.updateMessage(ctx, b.cfg.BotToken, channel, ts, text, blocks)
		b.promptMu.Lock()
		switch {
		case err == nil:
			p.fails = 0
			if p.dirty {
				b.promptMu.Unlock()
				continue
			}
			b.finishRedrawLocked(p)
		case ctx.Err() != nil:
			p.dirty = true
		case permanentEditError(err) || p.fails+1 >= maxRedrawFails:
			p.dirty = false
			log.Warnf("slack: gave up redrawing a prompt message: %v", err)
			b.finishRedrawLocked(p)
		default:
			p.dirty = true
			p.fails++
			p.retryAt = b.state.now().Add(b.backoff(p.fails))
			log.Warnf("slack: prompt message edit failed (will retry): %v", err)
		}
		b.promptMu.Unlock()
		return
	}
}

// finishRedrawLocked takes p off the dirty set after its last edit. The
// caller holds b.promptMu.
func (b *Bridge) finishRedrawLocked(p *prompt) {
	delete(b.promptDirty, p.id)
	if p.state == promptClosedState {
		scrubPromptLocked(p)
	}
}

// queueStaleEdit queues the removal of the buttons of a prompt message from
// before a restart, best effort: the proxy knows nothing of the prompt, so
// the message keeps its blocks and loses its actions.
func (b *Bridge) queueStaleEdit(p interactivePayload) {
	channel, ts := p.Container.ChannelID, p.Container.MessageTS
	if channel == "" {
		channel = p.Channel.ID
	}
	if ts == "" {
		ts = p.Message.TS
	}
	if channel == "" || ts == "" {
		return
	}
	var keep []json.RawMessage
	for _, blk := range gjson.ParseBytes(p.Message.Blocks).Array() {
		if blk.Get("type").String() != "actions" {
			keep = append(keep, json.RawMessage(blk.Raw))
		}
	}
	data, err := json.Marshal(append(keep, mustJSON(contextBlock(staleStatus))))
	if err != nil {
		return
	}
	text := p.Message.Text
	if text == "" {
		text = staleStatus
	}
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	if len(b.staleEdits) >= maxStaleEdits {
		return
	}
	for _, e := range b.staleEdits {
		if e.channel == channel && e.ts == ts {
			return
		}
	}
	b.staleEdits = append(b.staleEdits, staleEdit{channel: channel, ts: ts, text: text, blocks: string(data)})
	b.wakeRedraw()
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
}

// permanentEditError reports a chat.update failure no retry fixes.
func permanentEditError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.code {
	case "msg_too_long", "invalid_arguments", "invalid_blocks", "invalid_blocks_format",
		"message_not_found", "cant_update_message", "channel_not_found",
		"edit_window_closed", "is_archived", "not_in_channel":
		return true
	}
	return false
}

// answerPromptLocked records answer on an open prompt and wakes its waiters;
// it reports false when the prompt is no longer open. The caller holds
// b.promptMu.
func (b *Bridge) answerPromptLocked(p *prompt, answer agentbus.PromptAnswer) bool {
	if p.state != promptOpenState {
		return false
	}
	p.state, p.answer, p.answeredAt = promptAnsweredState, &answer, b.state.now()
	p.wakeLocked()
	b.markPromptDirtyLocked(p)
	return true
}

// promptEphemeral tells userID text privately, from a job: in channel when
// known, else in their DM with the bot.
func (b *Bridge) promptEphemeral(channel, userID, text string) {
	b.enqueueCommand(func(ctx context.Context) error {
		ch := channel
		if ch == "" {
			var err error
			if ch, err = b.dmChannel(ctx, userID); err != nil {
				return err
			}
		}
		return b.api.postEphemeral(ctx, b.cfg.BotToken, ch, userID, text)
	})
}

// splitPromptAction splits "prompt:<id>:<what>".
func splitPromptAction(actionID string) (id, what string, ok bool) {
	rest, found := strings.CutPrefix(actionID, promptActionPrefix)
	if !found {
		return "", "", false
	}
	id, what, found = strings.Cut(rest, ":")
	return id, what, found && id != "" && what != ""
}

// promptAction handles a click on a prompt message's button. Memory only: an
// owner's answer is recorded and the redraw queued; a modal opens in its own
// goroutine; refusals go out as ephemeral jobs.
func (b *Bridge) promptAction(p interactivePayload) {
	var actionID string
	for _, a := range p.Actions {
		if strings.HasPrefix(a.ActionID, promptActionPrefix) {
			actionID = a.ActionID
			break
		}
	}
	channel := p.Container.ChannelID
	if channel == "" {
		channel = p.Channel.ID
	}
	user, ok := b.state.user(p.User.ID)
	if !ok || !user.config {
		log.Infof("slack: refused a prompt click from %s: not an owner", p.User.ID)
		b.promptEphemeral(channel, p.User.ID, notOwnerAnswer)
		return
	}
	id, what, ok := splitPromptAction(actionID)
	if !ok {
		return
	}
	b.promptMu.Lock()
	b.initPromptsLocked()
	pr := b.promptByID[id]
	if pr == nil {
		// Not a prompt this proxy knows: the message is from before a restart.
		b.promptMu.Unlock()
		b.promptEphemeral(channel, user.ID, staleAnswer)
		b.queueStaleEdit(p)
		return
	}
	if pr.state != promptOpenState {
		b.promptMu.Unlock()
		b.promptEphemeral(channel, user.ID, alreadyAnswer)
		return
	}
	if opensPromptModal(pr.req, what) {
		view, err := b.promptModal(pr.snap())
		b.promptMu.Unlock()
		if err != nil {
			log.Warnf("slack: can't build the prompt modal: %v", err)
			return
		}
		b.openViewNow(user.ID, p.TriggerID, view)
		return
	}
	answer, valid := buttonAnswer(pr.req, what, user.Label)
	if !valid {
		b.promptMu.Unlock()
		log.Debugf("slack: ignored prompt action %q", what)
		return
	}
	answered := b.answerPromptLocked(pr, answer)
	b.promptMu.Unlock()
	if !answered {
		b.promptEphemeral(channel, user.ID, alreadyAnswer)
	}
}

// opensPromptModal reports whether button what of a req prompt opens a modal
// instead of answering.
func opensPromptModal(req agentbus.PromptRequest, what string) bool {
	switch req.Kind {
	case agentbus.PromptKindPermission:
		return what == "deny_reason"
	case agentbus.PromptKindPlan:
		return what == "keep_planning"
	case agentbus.PromptKindQuestion:
		return what == "answer"
	}
	return false
}

// buttonAnswer is the answer button what gives on a req prompt; valid is
// false for a button the prompt does not offer.
func buttonAnswer(req agentbus.PromptRequest, what, by string) (agentbus.PromptAnswer, bool) {
	switch req.Kind {
	case agentbus.PromptKindPermission:
		if what == "allow" || what == "deny" || (what == "always" && req.Always) {
			return agentbus.PromptAnswer{Choice: what, By: by}, true
		}
	case agentbus.PromptKindPlan:
		if what == "approve" || what == "approve_accept_edits" {
			return agentbus.PromptAnswer{Choice: what, By: by}, true
		}
	case agentbus.PromptKindDenial:
		if what == "allow_once" || what == "deny" {
			return agentbus.PromptAnswer{Choice: what, By: by}, true
		}
	case agentbus.PromptKindQuestion:
		idx, isOpt := strings.CutPrefix(what, "opt:")
		if !isOpt || !singleChoice(req) {
			break
		}
		n, err := strconv.Atoi(idx)
		q := req.Questions[0]
		if err != nil || n < 0 || n >= len(q.Options) {
			break
		}
		return agentbus.PromptAnswer{Choice: "submit", Answers: map[string]any{q.ID: q.Options[n].Label}, By: by}, true
	}
	return agentbus.PromptAnswer{}, false
}

// singleChoice reports whether req is one single-choice question, which gets
// a button per option.
func singleChoice(req agentbus.PromptRequest) bool {
	return req.Kind == agentbus.PromptKindQuestion && len(req.Questions) == 1 &&
		req.Questions[0].Kind == agentbus.QuestionChoice && !req.Questions[0].Multi
}

// modalErrors is the ack that shows errors (block id to text) in the modal.
func modalErrors(errs map[string]string) map[string]any {
	return map[string]any{"response_action": "errors", "errors": errs}
}

// promptSubmit handles the prompt modal's submit. A valid one from an owner
// answers the prompt and closes the modal; an invalid one shows why. When the
// prompt is gone the modal closes and the user is told so privately, since
// the modal may have no block to hang an error on.
func (b *Bridge) promptSubmit(p interactivePayload) map[string]any {
	id := strings.TrimSpace(p.View.PrivateMetadata)
	user, ok := b.state.user(p.User.ID)
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	b.initPromptsLocked()
	pr := b.promptByID[id]
	if !ok || !user.config {
		log.Infof("slack: refused a prompt submit from %s: not an owner", p.User.ID)
		b.promptEphemeral("", p.User.ID, notOwnerAnswer)
		if pr == nil {
			return nil
		}
		return modalErrors(map[string]string{firstPromptBlock(pr.req): notOwnerAnswer})
	}
	if pr == nil || pr.state != promptOpenState {
		b.promptEphemeral("", user.ID, noLongerOpen)
		return nil
	}
	answer, errs := parsePromptModal(pr.req, p.View.State.Values, user.Label)
	if len(errs) > 0 {
		return modalErrors(errs)
	}
	if !b.answerPromptLocked(pr, answer) {
		b.promptEphemeral("", user.ID, noLongerOpen)
	}
	return nil
}

// firstPromptBlock is the id of the first input block of req's modal.
func firstPromptBlock(req agentbus.PromptRequest) string {
	if req.Kind == agentbus.PromptKindQuestion {
		return questionBlock(0)
	}
	return "reason"
}

// parsePromptModal reads a submitted prompt modal into an answer, or the
// errors to show (block id to text).
func parsePromptModal(req agentbus.PromptRequest, values map[string]map[string]viewValue, by string) (agentbus.PromptAnswer, map[string]string) {
	switch req.Kind {
	case agentbus.PromptKindPermission, agentbus.PromptKindPlan:
		choice := "deny"
		if req.Kind == agentbus.PromptKindPlan {
			choice = "keep_planning"
		}
		reason := firstRunes(strings.TrimSpace(values["reason"]["reason"].Value), maxReasonRunes)
		return agentbus.PromptAnswer{Choice: choice, Reason: reason, By: by}, nil
	case agentbus.PromptKindQuestion:
		return parseQuestionModal(req, values, by)
	}
	return agentbus.PromptAnswer{}, map[string]string{"reason": "This prompt has no form."}
}

func parseQuestionModal(req agentbus.PromptRequest, values map[string]map[string]viewValue, by string) (agentbus.PromptAnswer, map[string]string) {
	answers := map[string]any{}
	errs := map[string]string{}
	for i, q := range req.Questions {
		block := questionBlock(i)
		v := values[block][block]
		switch q.Kind {
		case agentbus.QuestionChoice:
			other := firstRunes(strings.TrimSpace(values[block+otherSuffix][block+otherSuffix].Value), maxReasonRunes)
			if q.Multi {
				picks := []string{}
				for _, o := range v.SelectedOptions {
					if n, err := strconv.Atoi(o.Value); err == nil && n >= 0 && n < len(q.Options) {
						picks = append(picks, q.Options[n].Label)
					}
				}
				if other != "" {
					picks = append(picks, other)
				}
				if len(picks) == 0 {
					errs[block] = pickOrOther
					continue
				}
				answers[q.ID] = picks
				continue
			}
			label := ""
			if v.SelectedOption != nil {
				if n, err := strconv.Atoi(v.SelectedOption.Value); err == nil && n >= 0 && n < len(q.Options) {
					label = q.Options[n].Label
				}
			}
			switch {
			case other != "":
				answers[q.ID] = other
			case label != "":
				answers[q.ID] = label
			default:
				errs[block] = pickOrOther
			}
		case agentbus.QuestionText:
			text := firstRunes(strings.TrimSpace(v.Value), maxReasonRunes)
			if text == "" {
				errs[block] = "Write an answer."
				continue
			}
			answers[q.ID] = text
		case agentbus.QuestionNumber:
			n, msg := parseNumberAnswer(q, v.Value)
			if msg != "" {
				errs[block] = msg
				continue
			}
			answers[q.ID] = n
		}
	}
	if len(errs) > 0 {
		return agentbus.PromptAnswer{}, errs
	}
	return agentbus.PromptAnswer{Choice: "submit", Answers: answers, By: by}, nil
}

const pickOrOther = "Pick an option or write another answer."

// parseNumberAnswer reads a number question's input: an integer as an int64,
// anything else as a float64, or the error to show.
func parseNumberAnswer(q agentbus.PromptQuestion, raw string) (any, string) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, "Enter a number."
	}
	if q.Min != nil && f < *q.Min {
		return nil, "Must be at least " + formatNumber(*q.Min) + "."
	}
	if q.Max != nil && f > *q.Max {
		return nil, "Must be at most " + formatNumber(*q.Max) + "."
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return int64(f), ""
	}
	return f, ""
}
