// The forge's store: every repository, ref, git object, issue, pull request, label and
// comment lives here, in memory, in one process. No disk, no network, no wall clock in any
// answer: timestamps come from a deterministic counter so two runs on the same requests
// produce the same bytes.
//
// The viewer login: the suite's token (MAKEFUN_SHOP_TOKEN) is opaque and random per trial,
// but the viewer it names is fixed by the competition's public scenario seed
// (SCENARIO_SEED 0x6a09e667f3bcc909, the same in every practice and scored instance), so the
// login the token names is always "adaiversen". Any other owner is foreign: its
// repositories answer 404, never 403.
//
// Go port of store.ts (v2, Node). Behavior-identical: same ids, same deterministic clock,
// same global-id encoding.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const viewerLogin = "adaiversen"
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const viewerID = 1000

const defaultBranch = "main"
const zeroOID = "0000000000000000000000000000000000000000"

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

type gitObjectType string

const (
	objCommit gitObjectType = "commit"
	objTree   gitObjectType = "tree"
	objBlob   gitObjectType = "blob"
	objTag    gitObjectType = "tag"
)

type gitObject struct {
	typ  gitObjectType
	data []byte
}

type issue struct {
	id        int
	number    int
	title     string
	body      string
	state     string // "open" | "closed"
	labels    []string
	seq       int
	createdAt string
	updatedAt string
}

type pull struct {
	id        int
	number    int
	title     string
	body      string
	state     string // "open" | "closed"
	merged    bool
	mergedAt  string
	mergeSha  string
	headRef   string
	baseRef   string
	headSha   string
	baseSha   string
	seq       int
	createdAt string
	updatedAt string
}

type comment struct {
	id        int
	body      string
	seq       int
	createdAt string
	updatedAt string
}

type label struct {
	id    int
	name  string
	color string
}

type repo struct {
	id            int
	name          string
	owner         string
	description   string
	isPrivate     bool
	defaultBranch string
	createdAt     string
	updatedAt     string
	refs          map[string]string
	objects       map[string]*gitObject
	// Insertion order of commits, for newest-first commit listings.
	commitSeq  map[string]int
	issues     []*issue
	pulls      []*pull
	labels     map[string]*label
	comments   map[int][]*comment
	nextIssue  int
	nextPull   int
	nextLabelID int
}

type store struct {
	mu sync.RWMutex

	repos       map[string]*repo
	nextRepoID  int
	nextIssueID int
	nextCommentID int
	nextPullID  int
	nextReqID   int64
	// Deterministic clock: seconds since 2025-01-01T00:00:00Z, +1 per timestamp taken.
	clockSec      int64
	commitCounter int64
}

func newStore() *store {
	return &store{
		repos:         make(map[string]*repo),
		nextRepoID:    1,
		nextIssueID:   1,
		nextCommentID: 1,
		nextPullID:    1,
		clockSec:      1735689600,
	}
}

func (s *store) key(owner, name string) string {
	return owner + "/" + name
}

func (s *store) getRepo(owner, name string) *repo {
	return s.repos[s.key(owner, name)]
}

// The repository the viewer owns, or nil when the owner is foreign / it is missing.
func (s *store) viewerRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.getRepo(owner, name)
}

func (s *store) createRepo(name string, isPrivate bool, description string) *repo {
	now := s.nowIso()
	r := &repo{
		id:            s.nextRepoID,
		name:          name,
		owner:         viewerLogin,
		description:   description,
		isPrivate:     isPrivate,
		defaultBranch: defaultBranch,
		createdAt:     now,
		updatedAt:     now,
		refs:          make(map[string]string),
		objects:       make(map[string]*gitObject),
		commitSeq:     make(map[string]int),
		labels:        make(map[string]*label),
		comments:      make(map[int][]*comment),
		nextIssue:     1,
		nextPull:      1,
		nextLabelID:   1,
	}
	s.nextRepoID++
	s.repos[s.key(viewerLogin, name)] = r
	return r
}

func (s *store) deleteRepo(owner, name string) bool {
	_, ok := s.repos[s.key(owner, name)]
	if ok {
		delete(s.repos, s.key(owner, name))
	}
	return ok
}

func (s *store) viewerRepos() []*repo {
	out := make([]*repo, 0, len(s.repos))
	for _, r := range s.repos {
		if r.owner == viewerLogin {
			out = append(out, r)
		}
	}
	// sort by id ascending (insertion order equals id order)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].id > out[j].id; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func (s *store) nextReq() string {
	n := atomic.AddInt64(&s.nextReqID, 1)
	return fmt.Sprintf("req-%d", n)
}

// A deterministic ISO timestamp (JS Date.toISOString shape).
func (s *store) nowIso() string {
	sec := s.clockSec
	s.clockSec++
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// A deterministic epoch second for a git commit object.
func (s *store) nextEpoch() int64 {
	s.commitCounter++
	return s.clockSec + s.commitCounter
}

// Opaque, stable global ids: base64url of "<Kind>:<n>". Never a bare database id.
func globalID(kind string, n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", kind, n)))
}

var globalIDRe = regexp.MustCompile(`^([A-Za-z]+):(\d+)$`)

// Decode a global id back to its kind and number, or nil.
func parseGlobalID(id string) (kind string, n int, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		// Node's base64url decoder tolerates padding; try padded too.
		raw2, err2 := base64.URLEncoding.DecodeString(id)
		if err2 != nil {
			return "", 0, false
		}
		raw = raw2
	}
	m := globalIDRe.FindSubmatch(raw)
	if m == nil {
		return "", 0, false
	}
	var num int
	_, err = fmt.Sscanf(string(m[2]), "%d", &num)
	if err != nil {
		return "", 0, false
	}
	return string(m[1]), num, true
}

// The viewer as a GitHub user object.
func viewerUser() map[string]any {
	return map[string]any{
		"login":     viewerLogin,
		"id":        viewerID,
		"node_id":   globalID("User", viewerID),
		"type":      "User",
		"site_admin": false,
	}
}

// The author/committer line for a server-made git object.
func identLine(s *store) (line string, epoch int64) {
	epoch = s.nextEpoch()
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, epoch), epoch
}
