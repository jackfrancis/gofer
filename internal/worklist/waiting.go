package worklist

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"time"
)

// An item is WAITING when the ball is in someone else's court: the user had the last
// human word on it (a question to the author, a reply to a reviewer), or someone's
// approving or changes-requested review is the last word on an item the user did not
// author. Waiting is not a score: it says whose turn it is, not how much the item
// matters, so the radar lists waiting items in their own section after the rest,
// ranked the same way (Resolution.Waiting), and the bulk actions leave them alone.
//
// The timeline decides it (enrich: Signals.AwaitingOthersSince, Signals.LastWord). A
// model may then read the user's last word (TurnReading): "I'll take a look
// tomorrow" leaves the turn with the user although they spoke last. New activity
// always wins over a reading: an answer, a push or a new review request changes the
// last word, and a reading of another last word does not apply.

// LastWord is the user's own comment or review when it is the last human word on an
// item, the reason it is waiting on others. enrich records it from the timeline.
type LastWord struct {
	// ID names the comment or review on GitHub ("comment:<id>", "review:<id>").
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Decisive marks an approving or changes-requested review, which settles the
	// turn without a reading.
	Decisive bool `json:"decisive,omitempty"`
	// Mentions are the logins the text @-mentions, other than the user's own and
	// bots', in order of first mention, at most MaxMentions.
	Mentions []string `json:"mentions,omitempty"`
	// Text is the body, at most MaxLastWordBytes: a longer one keeps its start and its
	// end, where a closing question usually is.
	Text string `json:"text,omitempty"`
	// Digest identifies the whole body, taken before Text is cut, so an edit anywhere
	// in a long comment (its cut middle too) is a new version (Key).
	Digest string `json:"digest,omitempty"`
	// Head is the pull request's head commit when the last word was made: the commit a
	// review names, or for a comment the head enrich first saw with it. A later head
	// means the author pushed since, which answers it (AwaitingOthersSince unset). It
	// is not part of Key: anchoring a last word does not change what it says.
	Head string `json:"head,omitempty"`
	// Answered records that a push answered this version of the last word (the head
	// moved past Head). It stands until the last word changes, whatever a later read
	// of the head says, so a failed lookup cannot revive the wait.
	Answered bool `json:"answered,omitempty"`
}

const (
	// MaxLastWordBytes bounds LastWord.Text.
	MaxLastWordBytes = 4 << 10
	// MaxMentions bounds LastWord.Mentions.
	MaxMentions = 10
	// maxLogin is the longest GitHub login.
	maxLogin = 39
)

// NewLastWord returns a LastWord held to its bounds.
func NewLastWord(id string, at time.Time, decisive bool, mentions []string, text string) *LastWord {
	lw := &LastWord{ID: id, At: at, Decisive: decisive, Mentions: mentions, Text: text, Digest: digest(text)}
	lw.bound()
	return lw
}

// digestLen is the length of LastWord.Digest in hex digits.
const digestLen = 24

// digest returns a LastWord.Digest of text.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:digestLen/2])
}

// bound holds lw to MaxLastWordBytes and MaxMentions, keeping only well-formed logins.
func (lw *LastWord) bound() {
	lw.ID = truncateUTF8(lw.ID, projectedString)
	lw.Text = headTail(lw.Text, MaxLastWordBytes)
	if !validSHA(lw.Head) {
		lw.Head = ""
	}
	if len(lw.Digest) != digestLen || !validSHA(lw.Digest) { // hex
		lw.Digest = ""
	}
	var ms []string
	for _, m := range lw.Mentions {
		if validLogin(m) && !containsFold(ms, m) {
			ms = append(ms, m)
		}
		if len(ms) == MaxMentions {
			break
		}
	}
	lw.Mentions = ms
}

// Key identifies the last word as read: its ID and its whole body (Digest), so an
// edited comment reads as a new last word. One stored without a digest is keyed by
// its text.
func (lw LastWord) Key() string {
	d := lw.Digest
	if d == "" {
		d = digest(lw.Text)
	}
	return lw.ID + "#" + d
}

// TeamConfirmation records the team review requests outstanding when a refresh's
// review-requested:@me search confirmed one of them names the user (enrich: the hit
// had no other explanation), each by its team and when it was made. Which one names
// the user is unknown, so the confirmation stands only while every one of them is
// still outstanding (StillOutstanding): then a search that was cut short, or none at
// all, leaves it standing. A request made since is not covered, and does not end it.
type TeamConfirmation struct {
	Requests []ConfirmedRequest `json:"requests"`
}

// ConfirmedRequest is one team review request a TeamConfirmation covers.
type ConfirmedRequest struct {
	Team string    `json:"team"`
	At   time.Time `json:"at"`
}

const (
	// maxTeamRequests bounds TeamConfirmation.Requests.
	maxTeamRequests = 20
	// maxTeamSlug bounds ConfirmedRequest.Team.
	maxTeamSlug = 100
)

// NewTeamConfirmation returns a confirmation of requests in a stable order, or nil
// when there are none or more than it holds.
func NewTeamConfirmation(requests []ConfirmedRequest) *TeamConfirmation {
	c := &TeamConfirmation{Requests: slices.Clone(requests)}
	slices.SortFunc(c.Requests, func(a, b ConfirmedRequest) int {
		return cmp.Or(strings.Compare(a.Team, b.Team), a.At.Compare(b.At))
	})
	if !c.valid() {
		return nil
	}
	return c
}

// valid reports whether c is within its bounds: a confirmation with any malformed
// request is dropped whole, since dropping one request would widen what it covers.
func (c *TeamConfirmation) valid() bool {
	if len(c.Requests) == 0 || len(c.Requests) > maxTeamRequests {
		return false
	}
	for _, r := range c.Requests {
		if r.Team == "" || len(r.Team) > maxTeamSlug || r.At.IsZero() {
			return false
		}
	}
	return true
}

// StillOutstanding reports whether every request c covers is still outstanding:
// outstanding returns when the team's outstanding request was made, if it has one.
// A team asked again since holds a different request.
func (c *TeamConfirmation) StillOutstanding(outstanding func(team string) (time.Time, bool)) bool {
	if c == nil || !c.valid() {
		return false
	}
	for _, r := range c.Requests {
		if at, ok := outstanding(r.Team); !ok || !at.Equal(r.At) {
			return false
		}
	}
	return true
}

// Outcomes of a TurnReading.
const (
	// TurnWaitingOn: the last word asks or waits on someone else (WaitingOn names
	// them when the reading could tell).
	TurnWaitingOn = "waiting_on"
	// TurnMine: the user still owes the next step ("I'll take a look tomorrow").
	TurnMine = "my_turn"
	// TurnNothingPending: nobody owes anything ("LGTM, thanks!", "/lgtm").
	TurnNothingPending = "nothing_pending"
	// TurnUnclear: the reading could not tell; the timeline decides.
	TurnUnclear = "unclear"
)

// TurnReading is a model's reading of the user's last word (Signals.LastWord). It is
// a model-derived signal, kept across write-backs that carry none
// (KeepModelSignals), and it applies only while the last word it read is still the
// last word (CurrentTurnReading).
type TurnReading struct {
	// LastWord is the Key of the LastWord it read.
	LastWord string `json:"last_word"`
	// Outcome is one of TurnWaitingOn, TurnMine, TurnNothingPending, TurnUnclear;
	// anything else reads as TurnUnclear.
	Outcome string `json:"outcome"`
	// WaitingOn is who the last word waits on, for TurnWaitingOn. It counts only when
	// it is a login the last word mentions, or the item's author (Waiting).
	WaitingOn string    `json:"waiting_on,omitempty"`
	ReadAt    time.Time `json:"read_at"`
}

// bound holds r to the sizes a reading can have.
func (r *TurnReading) bound() {
	r.LastWord = truncateUTF8(r.LastWord, projectedString)
	switch r.Outcome {
	case TurnWaitingOn, TurnMine, TurnNothingPending, TurnUnclear:
	default:
		r.Outcome = TurnUnclear
	}
	r.WaitingOn = strings.TrimPrefix(strings.TrimSpace(r.WaitingOn), "@")
	if !validLogin(r.WaitingOn) {
		r.WaitingOn = ""
	}
}

// BoundTurnSignals holds an item's LastWord and TurnReading to their bounds, in place,
// whatever a run sent. RunPolicy.Apply calls it on every write-back.
func BoundTurnSignals(s *Signals) {
	if c := s.TeamConfirmed; c != nil {
		s.TeamConfirmed = NewTeamConfirmation(c.Requests)
	}
	if s.LastWord != nil {
		lw := *s.LastWord
		lw.Mentions = slices.Clone(lw.Mentions)
		lw.bound()
		s.LastWord = &lw
	}
	if s.Turn != nil {
		r := *s.Turn
		r.bound()
		s.Turn = &r
	}
}

// CurrentTurnReading returns the reading of the item's current last word, or nil
// when there is none, or it read a last word since replaced.
func (s Signals) CurrentTurnReading() *TurnReading {
	if s.Turn == nil || s.LastWord == nil || s.Turn.LastWord != s.LastWord.Key() {
		return nil
	}
	return s.Turn
}

// NeedsTurnReading reports whether a model should read the item's last word: the
// item is waiting on others because of a last word that is not decisive and has
// text, and no reading of it is stored.
func (s Signals) NeedsTurnReading() bool {
	lw := s.LastWord
	return !s.AwaitingOthersSince.IsZero() && s.AwaitingMeSince.IsZero() &&
		lw != nil && !lw.Decisive && strings.TrimSpace(lw.Text) != "" &&
		s.CurrentTurnReading() == nil
}

// Waiting describes an item waiting on someone other than the user.
type Waiting struct {
	// On is the login the item waits on; empty when it is not known.
	On string
	// Since is when the ball left the user's court.
	Since time.Time
	// NothingPending marks a last word that asks nothing of anyone ("LGTM, thanks!").
	NothingPending bool
}

// Waiting reports whether the item is waiting on someone other than the user, and on
// whom. It is a pure function of the item, so the radar decides it at read time:
//
//   - The timeline must say the ball is in others' court (Signals.AwaitingOthersSince,
//     and no review is awaiting the user).
//   - On the user's own item it must rest on their recorded last word: a request for
//     changes by someone else leaves the next step with the user, and an item stored
//     before LastWord was recorded may carry that older reading of the timeline until
//     enrich reads it again.
//   - A last word that is not a decisive review must have text: one whose words could
//     not be read (an inline-only review whose comments did not load) says nothing
//     about whose turn it is.
//   - A current reading (CurrentTurnReading) of the user's last word can keep the
//     item with the user (TurnMine), mark it as needing nothing (TurnNothingPending),
//     or name who it waits on (TurnWaitingOn). TurnUnclear leaves it to the timeline.
//   - Who it waits on is the reading's answer if the last word mentions that login or
//     it is the item's author; else the first login the last word mentions; else the
//     author of an item the user did not author; else unknown.
func (w WorkItem) Waiting() (Waiting, bool) {
	s := w.Signals
	if s.AwaitingOthersSince.IsZero() || !s.AwaitingMeSince.IsZero() {
		return Waiting{}, false
	}
	own := containsReason(s.Reasons, ReasonAuthor)
	lw := s.LastWord
	if own && lw == nil {
		return Waiting{}, false
	}
	if lw != nil && (lw.Answered || !lw.Decisive && strings.TrimSpace(lw.Text) == "") {
		return Waiting{}, false
	}
	out := Waiting{Since: s.AwaitingOthersSince}
	var mentions []string
	if lw != nil {
		mentions = lw.Mentions
	}
	author := w.GitHub.Author
	if own {
		author = "" // the user is the author: they are not who it waits on
	}
	if r := s.CurrentTurnReading(); r != nil {
		switch r.Outcome {
		case TurnMine:
			return Waiting{}, false
		case TurnNothingPending:
			out.NothingPending = true
			return out, true
		case TurnWaitingOn:
			if r.WaitingOn != "" && (containsFold(mentions, r.WaitingOn) || (author != "" && strings.EqualFold(r.WaitingOn, author))) {
				out.On = r.WaitingOn
				return out, true
			}
		}
	}
	switch {
	case len(mentions) > 0:
		out.On = mentions[0]
	case author != "":
		out.On = author
	}
	return out, true
}

// mentionRE matches an @-mention: an @ not preceded by a word character, backquote or
// slash (an email address, code, a URL path), then a login. A login followed by a
// slash is a team (@org/team), which names no one person; one followed by "[bot]" is
// a GitHub App's bot account (@dependabot[bot]), not the person whose login it starts
// with.
var mentionRE = regexp.MustCompile("(?:^|[^\\w`/@])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))(/|\\[bot\\])?")

// Mentions returns the logins text @-mentions, in order of first mention, without
// repeats. Quoted lines ("> …") and code (fenced blocks and `spans`) are left out:
// quoting someone, or writing @ in code, asks nothing of them.
func Mentions(text string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(t, ">") {
			continue
		}
		line = stripCodeSpans(line)
		for _, m := range mentionRE.FindAllStringSubmatch(line, -1) {
			if m[2] != "" {
				continue // a team, or a bot account
			}
			if !containsFold(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// stripCodeSpans blanks `code spans` in line.
func stripCodeSpans(line string) string {
	var b strings.Builder
	in := false
	for _, r := range line {
		if r == '`' {
			in = !in
			b.WriteRune(' ')
			continue
		}
		if in {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validSHA reports whether s has the shape of a git commit ID (SHA-1 or SHA-256).
func validSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// validLogin reports whether s has the shape of a GitHub login.
func validLogin(s string) bool {
	if s == "" || len(s) > maxLogin || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// headTail returns s if it fits in n bytes, else its start and end joined by an
// ellipsis line, n bytes in all, without splitting a character.
func headTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const sep = "\n…\n"
	head := truncateUTF8(s, n/4)
	tailBytes := n - len(head) - len(sep)
	i := len(s) - tailBytes
	for i < len(s) && !isRuneStart(s[i]) {
		i++
	}
	return head + sep + s[i:]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ItemTurn is one item's turn reading, as a reading pass writes it back.
type ItemTurn struct {
	ItemID  string      `json:"item_id"`
	Reading TurnReading `json:"reading"`
}

// MaxTurnBatch bounds the readings one ApplyTurns call (one agent-plane request)
// carries.
const MaxTurnBatch = 1000

// ApplyTurns sets the turn readings of ownerID's items, and nothing else, in one
// Store.Update (see SetTurn): a pass's readings land in one transaction, not one
// whole-worklist read and write each. A reading pass reads the items, asks the
// model, and writes back only the readings, so whatever landed meanwhile (a refresh,
// a hide or unhide) stands. A reading for an item that is gone, or that SetTurn
// drops, is skipped without error; only the items it changed are written.
func ApplyTurns(ctx context.Context, store Store, ownerID string, turns []ItemTurn) error {
	if len(turns) == 0 {
		return nil
	}
	byID := make(map[string]TurnReading, len(turns))
	for _, t := range turns {
		byID[t.ItemID] = t.Reading
	}
	return store.Update(ctx, ownerID, func(current []WorkItem) ([]WorkItem, error) {
		var changed []WorkItem
		for i := range current {
			if r, ok := byID[current[i].ID]; ok && SetTurn(&current[i], r) {
				changed = append(changed, current[i])
			}
		}
		return changed, nil
	})
}

// SetTurn sets item's turn reading, held to its bounds, and reports whether it did. A
// reading is dropped unless it read the item's current last word (the key matches
// Signals.LastWord), and for a hidden item, which carries no signals.
func SetTurn(item *WorkItem, r TurnReading) bool {
	r.bound()
	if item.IsHidden() || item.Signals.LastWord == nil || r.LastWord != item.Signals.LastWord.Key() {
		return false
	}
	item.Signals.Turn = &r
	return true
}

// TurnReader reads the user's last word on a waiting item (Signals.LastWord) and says
// whose turn it is. The real implementation calls a chat model from an agent runtime;
// gofer core depends only on this interface. The reading it returns need not set
// LastWord or ReadAt: the caller stamps both.
type TurnReader interface {
	ReadTurn(ctx context.Context, item WorkItem) (TurnReading, error)
}
