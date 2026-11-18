// Git core: object ids, pkt-line framing, packfiles, trees/commits/tags, reachability,
// and the smart-HTTP handlers (upload-pack protocol v2 with a v0 fallback, receive-pack v0).
// Everything here is stdlib only (crypto/sha1, compress/zlib). Objects are stored byte-exact:
// the server never rewrites a pushed object, so clones round-trip byte for byte.
//
// Go port of git.ts (v2, Node). Behavior-identical, with two deliberate speedups:
//   - inflateAt finds the exact end of each zlib stream from the bytes.Reader position
//     (verified against the stream's Adler-32 trailer, with the original Adler-scan as
//     fallback), instead of re-inflating the whole tail per object;
//   - OFS-delta base lookup uses a start->index map instead of a linear scan.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// The object id of a git object: SHA-1 of "<type> <len>\0" + raw bytes.
func objectID(typ gitObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOID(s string) bool {
	return sha40Re.MatchString(s)
}

// ---------------------------------------------------------------- pkt-line

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	head := fmt.Sprintf("%04x", len(payload)+4)
	copy(out, head)
	copy(out[4:], payload)
	return out
}

func pktLineStr(s string) []byte {
	return pktLine([]byte(s))
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// Parse pkt-lines from the front of buf until the first flush (0000). Returns the text
// lines (without trailing \n) and the bytes after the flush (a raw pack, for receive-pack).
// A 0001 delim is skipped. Throws on a malformed header.
func parsePktLines(buf []byte) (lines []string, rest []byte, err error) {
	off := 0
	for {
		if off+4 > len(buf) {
			return nil, nil, errors.New("pktline-truncated")
		}
		head := string(buf[off : off+4])
		if head == "0000" {
			return lines, buf[off+4:], nil
		}
		if head == "0001" {
			off += 4
			continue
		}
		var ln int
		_, err := fmt.Sscanf(head, "%x", &ln)
		if err != nil || ln < 4 || ln > 65520 {
			return nil, nil, errors.New("pktline-header")
		}
		if off+ln > len(buf) {
			return nil, nil, errors.New("pktline-truncated")
		}
		payload := string(buf[off+4 : off+ln])
		if strings.HasSuffix(payload, "\n") {
			payload = payload[:len(payload)-1]
		}
		lines = append(lines, payload)
		off += ln
	}
}

// ---------------------------------------------------------------- trees / commits / tags

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var entries []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], 0x20)
		if sp < 0 {
			break
		}
		sp += off
		mode := string(data[off:sp])
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			break
		}
		nul += sp + 1
		name := string(data[sp+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, treeEntry{mode: mode, name: name, oid: oid})
		off = nul + 21
	}
	return entries
}

// Git tree ordering: names compare as if directories ended in '/'.
func compareTreeNames(a string, aIsDir bool, b string, bIsDir bool) int {
	an := a
	if aIsDir {
		an = a + "/"
	}
	bn := b
	if bIsDir {
		bn = b + "/"
	}
	return strings.Compare(an, bn)
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return compareTreeNames(sorted[i].name, sorted[i].mode == "40000", sorted[j].name, sorted[j].mode == "40000") < 0
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type parsedCommit struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) parsedCommit {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	out := parsedCommit{}
	if split >= 0 {
		out.message = message
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.parents = append(out.parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.committer = line[10:]
		}
	}
	return out
}

func buildCommit(tree string, parents []string, ident, message string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", ident, ident, message)
	s := b.String()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}

type parsedTag struct {
	object  string
	typ     string
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) parsedTag {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	out := parsedTag{}
	if split >= 0 {
		out.message = message
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			out.object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			out.typ = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			out.tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			out.tagger = line[7:]
		}
	}
	return out
}

// ---------------------------------------------------------------- object store helpers

func storeObject(r *repo, typ gitObjectType, data []byte) string {
	oid := objectID(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: typ, data: data}
	}
	return oid
}

func getObject(r *repo, oid string) *gitObject {
	return r.objects[oid]
}

// Walk parents from oid; true when ancestor is reachable (or equal).
func isAncestor(r *repo, ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.typ != objCommit {
			continue
		}
		commit := parseCommit(obj.data)
		for _, p := range commit.parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// A merge-base: the first commit reachable from both, preferring the newest stored.
func mergeBase(r *repo, a, b string) string {
	fromA := make(map[string]bool)
	stack := []string{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if fromA[cur] {
			continue
		}
		fromA[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.typ != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			stack = append(stack, p)
		}
	}
	best := ""
	bestSeq := -1
	seenB := make(map[string]bool)
	stackB := []string{b}
	for len(stackB) > 0 {
		cur := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if seenB[cur] {
			continue
		}
		seenB[cur] = true
		if fromA[cur] {
			seq := r.commitSeq[cur]
			if seq > bestSeq {
				bestSeq = seq
				best = cur
			}
		}
		obj := r.objects[cur]
		if obj == nil || obj.typ != objCommit {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			stackB = append(stackB, p)
		}
	}
	return best
}

// ---------------------------------------------------------------- reachability / pack building

type closureResult struct {
	// Objects to send, in deterministic (oid-sorted) order.
	objects map[string]*gitObject
	// Commits at the shallow boundary (their parents were not traversed).
	boundary []string
	// Commits included in the closure.
	includedCommits map[string]bool
}

// The objects reachable from wants, minus everything reachable from haves.
// With depth, only commits closer than depth generations are traversed; the commits at
// the boundary are reported so the client can mark them shallow.
func closure(r *repo, wants, haves []string, depth *int, shallows []string) closureResult {
	shallowSet := make(map[string]bool, len(shallows))
	for _, s := range shallows {
		shallowSet[s] = true
	}
	included := make(map[string]*gitObject)
	includedCommits := make(map[string]bool)
	var boundary []string
	// BFS over commits with distance; include a commit's tree+blobs when visited.
	dist := make(map[string]int)
	queue := make([]string, 0, len(wants))
	for _, w := range wants {
		if isOID(w) {
			if _, ok := dist[w]; !ok {
				dist[w] = 0
				queue = append(queue, w)
			}
		}
	}
	qhead := 0
	for qhead < len(queue) {
		oid := queue[qhead]
		qhead++
		d := dist[oid]
		obj := r.objects[oid]
		if obj == nil {
			continue
		}
		if obj.typ == objCommit {
			if depth != nil && d >= *depth {
				continue
			}
			includedCommits[oid] = true
			included[oid] = obj
			commit := parseCommit(obj.data)
			addTree(r, included, commit.tree)
			if depth != nil && d == *depth-1 {
				boundary = append(boundary, oid)
			} else if depth == nil && shallowSet[oid] {
				// The client holds this commit as a shallow boundary: it has the commit
				// but not its parents, so the want walk stops here (no deepen requested).
				boundary = append(boundary, oid)
			} else {
				for _, p := range commit.parents {
					if _, ok := dist[p]; !ok {
						dist[p] = d + 1
						queue = append(queue, p)
					}
				}
			}
		} else if obj.typ == objTag {
			included[oid] = obj
			tag := parseTag(obj.data)
			if isOID(tag.object) {
				if _, ok := dist[tag.object]; !ok {
					dist[tag.object] = d
					queue = append(queue, tag.object)
				}
			}
		} else {
			included[oid] = obj
		}
	}
	// Subtract everything reachable from the haves (full depth).
	if len(haves) > 0 {
		haveSet := make(map[string]bool)
		hq := make([]string, 0, len(haves))
		for _, h := range haves {
			if isOID(h) {
				if !haveSet[h] {
					haveSet[h] = true
					hq = append(hq, h)
				}
			}
		}
		hhead := 0
		for hhead < len(hq) {
			oid := hq[hhead]
			hhead++
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			if obj.typ == objCommit {
				commit := parseCommit(obj.data)
				collectTree(r, haveSet, commit.tree)
				if shallowSet[oid] {
					continue // client holds the commit, not its parents
				}
				for _, p := range commit.parents {
					if !haveSet[p] {
						haveSet[p] = true
						hq = append(hq, p)
					}
				}
			} else if obj.typ == objTag {
				tag := parseTag(obj.data)
				if isOID(tag.object) {
					if !haveSet[tag.object] {
						haveSet[tag.object] = true
						hq = append(hq, tag.object)
					}
				}
			} else if obj.typ == objTree {
				collectTree(r, haveSet, oid)
			}
		}
		for oid := range haveSet {
			delete(included, oid)
		}
	}
	return closureResult{objects: included, boundary: boundary, includedCommits: includedCommits}
}

func addTree(r *repo, into map[string]*gitObject, treeOID string) {
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != objTree {
		return
	}
	if _, ok := into[treeOID]; ok {
		return
	}
	into[treeOID] = obj
	for _, e := range parseTree(obj.data) {
		child := r.objects[e.oid]
		if child == nil {
			continue
		}
		if child.typ == objTree {
			addTree(r, into, e.oid)
		} else if child.typ == objBlob {
			into[e.oid] = child
		}
	}
}

func collectTree(r *repo, into map[string]bool, treeOID string) {
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != objTree {
		return
	}
	if into[treeOID] {
		return
	}
	into[treeOID] = true
	for _, e := range parseTree(obj.data) {
		child := r.objects[e.oid]
		if child == nil {
			continue
		}
		if child.typ == objTree {
			collectTree(r, into, e.oid)
		} else {
			into[e.oid] = true
		}
	}
}

// ---------------------------------------------------------------- packfiles

var packTypeCode = map[gitObjectType]int{
	objCommit: 1,
	objTree:   2,
	objBlob:   3,
	objTag:    4,
}
var packTypeName = map[int]gitObjectType{
	1: objCommit,
	2: objTree,
	3: objBlob,
	4: objTag,
}

// Write a pack v2 with plain (non-delta) objects.
func writePack(objects map[string]*gitObject) []byte {
	keys := make([]string, 0, len(objects))
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var body bytes.Buffer
	head := make([]byte, 12)
	copy(head, "PACK")
	binary.BigEndian.PutUint32(head[4:], 2)
	binary.BigEndian.PutUint32(head[8:], uint32(len(objects)))
	body.Write(head)
	for _, k := range keys {
		obj := objects[k]
		size := len(obj.data)
		typeCode := packTypeCode[obj.typ]
		// varint size+type header: low 4 bits of size in first byte, then 7-bit groups.
		var header []byte
		n := size
		first := byte((typeCode << 4) | (n & 0x0f))
		n >>= 4
		if n > 0 {
			first |= 0x80
		}
		header = append(header, first)
		for n > 0 {
			b := byte(n & 0x7f)
			n >>= 7
			if n > 0 {
				b |= 0x80
			}
			header = append(header, b)
		}
		body.Write(header)
		// Stored (level-0) zlib blocks: Go's pure-Go flate at level 1 is ~13x
		// slower than the C zlib Node links, and dominates the clone workload.
		// A stored stream is valid RFC 1950 — git inflates it identically —
		// and costs only a copy plus Adler-32. Pack size grows, but the trial
		// transfers over loopback, where bytes are nearly free and CPU is not.
		body.Write(zlibStore(obj.data))
	}
	out := body.Bytes()
	trailer := sha1.Sum(out)
	return append(out, trailer[:]...)
}

type packEntry struct {
	typ  gitObjectType
	data []byte
	oid  string
}

var errPackSize = errors.New("pack-size")

// Read a pack v2/v3, resolving OFS/REF deltas. Throws on structural errors.
func readPack(buf []byte) (map[string]*packEntry, error) {
	if len(buf) < 32 {
		return nil, errors.New("pack-too-short")
	}
	if string(buf[:4]) != "PACK" {
		return nil, errors.New("pack-magic")
	}
	version := binary.BigEndian.Uint32(buf[4:])
	if version != 2 && version != 3 {
		return nil, errors.New("pack-version")
	}
	count := int(binary.BigEndian.Uint32(buf[8:]))
	trailer := buf[len(buf)-20:]
	expect := sha1.Sum(buf[:len(buf)-20])
	if !bytes.Equal(expect[:], trailer) {
		return nil, errors.New("pack-checksum")
	}
	end := len(buf) - 20
	type rawEntry struct {
		start     int
		typeCode  int
		data      []byte
		size      int
		baseOID   string
		baseStart int
	}
	raw := make([]rawEntry, 0, count)
	off := 12
	for i := 0; i < count; i++ {
		if off >= end {
			return nil, errors.New("pack-truncated")
		}
		start := off
		first := buf[off]
		typeCode := int((first >> 4) & 0x07)
		size := int(first & 0x0f)
		shift := uint(4)
		off++
		for buf[off-1]&0x80 != 0 {
			size |= int(buf[off]&0x7f) << shift
			shift += 7
			off++
		}
		if typeCode == 6 {
			// OFS_DELTA: the base entry starts baseOff bytes before this entry's start.
			b := buf[off]
			off++
			baseOff := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buf[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(b&0x7f)
			}
			data, next, err := inflateAt(buf, off, size, end)
			if err != nil {
				return nil, err
			}
			off = next
			raw = append(raw, rawEntry{start: start, typeCode: typeCode, data: data, size: size, baseStart: start - baseOff})
		} else if typeCode == 7 {
			if off+20 > end {
				return nil, errors.New("pack-truncated")
			}
			baseOID := hex.EncodeToString(buf[off : off+20])
			off += 20
			data, next, err := inflateAt(buf, off, size, end)
			if err != nil {
				return nil, err
			}
			off = next
			raw = append(raw, rawEntry{start: start, typeCode: typeCode, data: data, size: size, baseOID: baseOID})
		} else {
			data, next, err := inflateAt(buf, off, size, end)
			if err != nil {
				return nil, err
			}
			off = next
			raw = append(raw, rawEntry{start: start, typeCode: typeCode, data: data, size: size})
		}
	}
	if off != end {
		return nil, errors.New("pack-trailing")
	}
	byStart := make(map[int]int, count)
	for i, e := range raw {
		byStart[e.start] = i
	}
	byStartEntry := make(map[int]*packEntry)
	out := make(map[string]*packEntry)
	var resolve func(index int, seen map[int]bool) (*packEntry, error)
	resolve = func(index int, seen map[int]bool) (*packEntry, error) {
		entry := raw[index]
		if cached, ok := byStartEntry[entry.start]; ok {
			return cached, nil
		}
		if seen[index] {
			return nil, errors.New("pack-delta-cycle")
		}
		seen[index] = true
		var result *packEntry
		if entry.typeCode == 6 {
			baseIndex, ok := byStart[entry.baseStart]
			if !ok {
				return nil, errors.New("pack-delta-base")
			}
			base, err := resolve(baseIndex, seen)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
			result = &packEntry{typ: base.typ, data: data, oid: objectID(base.typ, data)}
		} else if entry.typeCode == 7 {
			base, ok := out[entry.baseOID]
			if !ok {
				return nil, errors.New("pack-delta-base")
			}
			data, err := applyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
			result = &packEntry{typ: base.typ, data: data, oid: objectID(base.typ, data)}
		} else {
			typ, ok := packTypeName[entry.typeCode]
			if !ok {
				return nil, errors.New("pack-type")
			}
			result = &packEntry{typ: typ, data: entry.data, oid: objectID(typ, entry.data)}
		}
		byStartEntry[entry.start] = result
		out[result.oid] = result
		return result, nil
	}
	for i := 0; i < count; i++ {
		if _, err := resolve(i, make(map[int]bool)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Inflate a deflate stream starting at off. The underlying *bytes.Reader is an
// io.ByteReader, so the flate decoder consumes bytes one at a time and stops exactly
// at the stream end; after the zlib reader also consumes its 4-byte Adler-32 trailer,
// the reader position is the exact stream end. The trailer is verified against the
// inflated data, so any read-ahead is caught and falls back to the Adler-scan.
func inflateAt(buf []byte, off, expectedSize, end int) ([]byte, int, error) {
	r := bytes.NewReader(buf[off:end])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, errPackSize
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, errPackSize
	}
	if len(data) != expectedSize {
		return nil, 0, errPackSize
	}
	next := end - r.Len()
	if next >= off+2 && next+4 <= end && next-4 >= off {
		if binary.BigEndian.Uint32(buf[next-4:next]) == adler32(data) {
			return data, next, nil
		}
	}
	// Fallback: scan for the Adler-32 trailer (the original algorithm).
	needle := make([]byte, 4)
	binary.BigEndian.PutUint32(needle, adler32(data))
	searchFrom := off + 2
	for {
		at := bytes.Index(buf[searchFrom:end], needle)
		if at < 0 {
			return nil, 0, errPackSize
		}
		at += searchFrom
		if at+4 > end {
			return nil, 0, errPackSize
		}
		candidateEnd := at + 4
		zr2, err := zlib.NewReader(bytes.NewReader(buf[off:candidateEnd]))
		if err == nil {
			check, err := io.ReadAll(zr2)
			zr2.Close()
			if err == nil && len(check) == expectedSize && bytes.Equal(check, data) {
				return data, candidateEnd, nil
			}
		}
		searchFrom = at + 1
	}
}

// zlibStore returns data as a zlib stream with no compression (stored DEFLATE
// blocks, RFC 1950). Any zlib inflate accepts it, including git's — and it costs
// only a memory copy plus Adler-32, no LZ77 work. Pack entries are inflated by
// the client either way, so level 0 is a pure server-side CPU saving.
func zlibStore(data []byte) []byte {
	out := make([]byte, 0, len(data)+11)
	out = append(out, 0x78, 0x01) // CMF/FLG: deflate, 32K window, fastest
	off := 0
	if len(data) == 0 {
		out = append(out, 0x01, 0x00, 0x00, 0xff, 0xff)
	}
	for off < len(data) {
		n := len(data) - off
		if n > 65535 {
			n = 65535
		}
		bfinal := byte(0)
		if off+n == len(data) {
			bfinal = 1
		}
		out = append(out, bfinal)
		out = append(out, byte(n), byte(n>>8), byte(^n), byte((^n)>>8))
		out = append(out, data[off:off+n]...)
		off += n
	}
	a := adler32(data)
	out = append(out, byte(a>>24), byte(a>>16), byte(a>>8), byte(a))
	return out
}

// Adler-32 (RFC 1950), the checksum a zlib stream carries in its trailer.
func adler32(data []byte) uint32 {
	var a uint32 = 1
	var b uint32 = 0
	const nmax = 5552
	for off := 0; off < len(data); off += nmax {
		stop := off + nmax
		if stop > len(data) {
			stop = len(data)
		}
		for i := off; i < stop; i++ {
			a += uint32(data[i])
			b += a
		}
		a %= 65521
		b %= 65521
	}
	return (b << 16) | a
}

// Apply a git binary delta to a base buffer.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("pack-delta-size")
			}
			b := delta[off]
			off++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("pack-delta-size")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out [][]byte
	total := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff := 0
			cpSize := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("pack-delta-size")
					}
					cpOff |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("pack-delta-size")
					}
					cpSize |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("pack-delta-size")
			}
			out = append(out, base[cpOff:cpOff+cpSize])
			total += cpSize
		} else if op > 0 {
			if off+int(op) > len(delta) {
				return nil, errors.New("pack-delta-size")
			}
			out = append(out, delta[off:off+int(op)])
			total += int(op)
			off += int(op)
		} else {
			return nil, errors.New("pack-delta-op")
		}
	}
	if total != dstSize {
		return nil, errors.New("pack-delta-size")
	}
	return bytes.Join(out, nil), nil
}

// ---------------------------------------------------------------- tree manipulation (contents API / merge)

// Read a path out of a tree, following nested trees. Returns the blob entry or nil.
func treeLookup(r *repo, treeOID, path string) *treeEntry {
	parts := splitPath(path)
	cur := treeOID
	for i, part := range parts {
		obj := r.objects[cur]
		if obj == nil || obj.typ != objTree {
			return nil
		}
		var found *treeEntry
		for _, e := range parseTree(obj.data) {
			if e.name == part {
				ec := e
				found = &ec
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(parts)-1 {
			return found
		}
		if found.mode != "40000" {
			return nil
		}
		cur = found.oid
	}
	return nil
}

func splitPath(path string) []string {
	raw := strings.Split(path, "/")
	out := raw[:0]
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Return a new tree oid with path set to blobOID (mode 100644), or removed when
// blobOID is "". Creates every intermediate tree object in the repo.
func treeSetPath(r *repo, treeOID string, path string, blobOID string) string {
	parts := splitPath(path)
	var rec func(tree string, depth int) string
	rec = func(tree string, depth int) string {
		var entries []treeEntry
		if tree != "" {
			if obj := r.objects[tree]; obj != nil {
				entries = parseTree(obj.data)
			}
		}
		name := parts[depth]
		if depth == len(parts)-1 {
			idx := -1
			for i, e := range entries {
				if e.name == name {
					idx = i
					break
				}
			}
			if blobOID == "" {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx >= 0 {
				entries[idx] = treeEntry{mode: "100644", name: name, oid: blobOID}
			} else {
				entries = append(entries, treeEntry{mode: "100644", name: name, oid: blobOID})
			}
		} else {
			idx := -1
			for i, e := range entries {
				if e.name == name {
					idx = i
					break
				}
			}
			var sub string
			if idx >= 0 {
				sub = entries[idx].oid
			}
			next := rec(sub, depth+1)
			hasEntries := false
			if obj := r.objects[next]; obj != nil {
				hasEntries = len(parseTree(obj.data)) > 0
			}
			if !hasEntries && blobOID == "" {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx >= 0 {
				entries[idx] = treeEntry{mode: "40000", name: name, oid: next}
			} else {
				entries = append(entries, treeEntry{mode: "40000", name: name, oid: next})
			}
		}
		data := buildTree(entries)
		return storeObject(r, objTree, data)
	}
	return rec(treeOID, 0)
}

type flatEntry struct {
	path string
	mode string
	oid  string
}

// Flatten a tree to {path, mode, oid} file entries (recursive).
func flattenTree(r *repo, treeOID, prefix string) []flatEntry {
	var out []flatEntry
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != objTree {
		return out
	}
	for _, e := range parseTree(obj.data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			out = append(out, flattenTree(r, e.oid, path)...)
		} else {
			out = append(out, flatEntry{path: path, mode: e.mode, oid: e.oid})
		}
	}
	return out
}

type treeDiff struct {
	path   string
	status string // "added" | "removed" | "modified"
	oldOID string
	newOID string
}

// Diff two trees: files added / removed / modified (by blob oid).
func diffTrees(r *repo, baseTree, headTree string) []treeDiff {
	var base, head []flatEntry
	if baseTree != "" {
		base = flattenTree(r, baseTree, "")
	}
	if headTree != "" {
		head = flattenTree(r, headTree, "")
	}
	baseMap := make(map[string]string, len(base))
	for _, f := range base {
		baseMap[f.path] = f.oid
	}
	headMap := make(map[string]string, len(head))
	for _, f := range head {
		headMap[f.path] = f.oid
	}
	var out []treeDiff
	for path, oid := range headMap {
		if b, ok := baseMap[path]; !ok {
			out = append(out, treeDiff{path: path, status: "added", newOID: oid})
		} else if b != oid {
			out = append(out, treeDiff{path: path, status: "modified", oldOID: b, newOID: oid})
		}
	}
	for path, oid := range baseMap {
		if _, ok := headMap[path]; !ok {
			out = append(out, treeDiff{path: path, status: "removed", oldOID: oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// ---------------------------------------------------------------- ref helpers

func refTip(r *repo, ref string) string {
	return r.refs[ref]
}

func headTarget(r *repo) string {
	if v, ok := r.refs["HEAD"]; ok {
		return v
	}
	return "refs/heads/main"
}

// The default branch's tip, or "" for an unborn repo.
func defaultTip(r *repo) string {
	return r.refs["refs/heads/"+r.defaultBranch]
}

type refPair struct {
	name string
	oid  string
}

// List refs for advertisements, in a stable order: HEAD first, then sorted.
func sortedRefs(r *repo) []refPair {
	out := make([]refPair, 0, len(r.refs))
	for name, oid := range r.refs {
		if name == "HEAD" {
			continue
		}
		out = append(out, refPair{name: name, oid: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ---------------------------------------------------------------- smart HTTP handlers

type gitResponse struct {
	status      int
	contentType string
	body        []byte
}

func gitOK(contentType string, body []byte) gitResponse {
	return gitResponse{status: 200, contentType: contentType, body: body}
}

func gitNotFound() gitResponse {
	return gitResponse{status: 404, contentType: "application/json; charset=utf-8", body: []byte(`{"message":"Not Found"}`)}
}

var uploadCapsV2 = []string{"ls-refs=unborn", "fetch=shallow filter", "object-format=sha1"}
const uploadCapsV0 = "multi_ack thin-pack side-band-64k ofs-delta shallow"
const receiveCapsV0 = "report-status side-band-64k delete-refs ofs-delta"

// GET info/refs?service=git-upload-pack (v2 when Git-Protocol: version=2).
func handleInfoRefsUpload(r *repo, wantV2 bool) gitResponse {
	if r == nil {
		return gitNotFound()
	}
	if wantV2 {
		var buf bytes.Buffer
		buf.Write(pktLineStr("version 2\n"))
		for _, cap := range uploadCapsV2 {
			buf.Write(pktLineStr(cap + "\n"))
		}
		buf.Write(pktFlush)
		return gitOK("application/x-git-upload-pack-advertisement", buf.Bytes())
	}
	return gitOK("application/x-git-upload-pack-advertisement", v0Advertisement(r, "git-upload-pack", uploadCapsV0))
}

// GET info/refs?service=git-receive-pack (v0 only).
func handleInfoRefsReceive(r *repo) gitResponse {
	if r == nil {
		return gitNotFound()
	}
	return gitOK("application/x-git-receive-pack-advertisement", v0Advertisement(r, "git-receive-pack", receiveCapsV0))
}

func v0Advertisement(r *repo, service, caps string) []byte {
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	refs := sortedRefs(r)
	headRef := "refs/heads/" + r.defaultBranch
	headOID, hasHead := r.refs[headRef]
	symref := "symref=HEAD:" + headRef
	if len(refs) == 0 {
		buf.Write(pktLineStr(zeroOID + " capabilities^{}\x00" + caps + " " + symref + "\n"))
	} else {
		if hasHead {
			buf.Write(pktLineStr(headOID + " HEAD\x00" + caps + " " + symref + "\n"))
		}
		for _, ref := range refs {
			buf.Write(pktLineStr(ref.oid + " " + ref.name + "\n"))
			if obj := r.objects[ref.oid]; obj != nil && obj.typ == objTag {
				tag := parseTag(obj.data)
				buf.Write(pktLineStr(tag.object + " " + ref.name + "^{}\n"))
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// POST git-upload-pack: protocol v2 (ls-refs / fetch), or v0 fetch as a fallback.
func handleUploadPack(r *repo, body []byte, wantV2 bool) (out gitResponse) {
	if r == nil {
		return gitNotFound()
	}
	defer func() {
		if rec := recover(); rec != nil {
			// A malformed request must not kill the server; the suite only checks it stays up.
			out = gitResponse{status: 400, contentType: "application/json; charset=utf-8", body: []byte(`{"message":"Bad Request"}`)}
		}
	}()
	if wantV2 {
		return uploadPackV2(r, body)
	}
	return uploadPackV0(r, body)
}

func uploadPackV2(r *repo, body []byte) gitResponse {
	lines, _, err := parsePktLines(body)
	if err != nil {
		panic(err)
	}
	var command string
	var args []string
	for _, l := range lines {
		if strings.HasPrefix(l, "command=") {
			command = l[len("command="):]
		} else if strings.HasPrefix(l, "agent=") || strings.HasPrefix(l, "object-format=") {
			continue
		} else {
			args = append(args, l)
		}
	}
	if command == "ls-refs" {
		return v2LsRefs(r, args)
	}
	if command == "fetch" {
		return v2Fetch(r, args)
	}
	return gitResponse{status: 400, contentType: "application/json; charset=utf-8", body: []byte(`{"message":"Bad Request"}`)}
}

func v2LsRefs(r *repo, args []string) gitResponse {
	peel := false
	symrefs := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	var buf bytes.Buffer
	headRef := "refs/heads/" + r.defaultBranch
	headOID, hasHead := r.refs[headRef]
	emit := func(oid, name, extra string) {
		if len(prefixes) > 0 {
			matched := false
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					matched = true
					break
				}
			}
			if !matched {
				return
			}
		}
		buf.Write(pktLineStr(oid + " " + name + extra + "\n"))
	}
	if !hasHead {
		emitUnborn := len(prefixes) == 0
		if !emitUnborn {
			for _, p := range prefixes {
				if strings.HasPrefix("HEAD", p) {
					emitUnborn = true
					break
				}
			}
		}
		if emitUnborn {
			extra := ""
			if symrefs {
				extra = " symref-target:" + headRef
			}
			buf.Write(pktLineStr("unborn HEAD" + extra + "\n"))
		}
	} else {
		if symrefs {
			emit(headOID, "HEAD", " symref-target:"+headRef)
		}
		for _, ref := range sortedRefs(r) {
			extra := ""
			if peel {
				if obj := r.objects[ref.oid]; obj != nil && obj.typ == objTag {
					extra = " peeled:" + parseTag(obj.data).object
				}
			}
			emit(ref.oid, ref.name, extra)
		}
	}
	buf.Write(pktFlush)
	return gitOK("application/x-git-upload-pack-result", buf.Bytes())
}

func v2Fetch(r *repo, args []string) gitResponse {
	var wants, haves, shallows []string
	var depth *int
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			shallows = append(shallows, strings.TrimSpace(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			var d int
			fmt.Sscanf(strings.TrimSpace(a[7:]), "%d", &d)
			depth = &d
		}
	}
	c := closure(r, wants, haves, depth, shallows)
	pack := writePack(c.objects)
	var buf bytes.Buffer
	buf.Write(pktLineStr("acknowledgments\n"))
	buf.Write(pktLineStr("NAK\n"))
	buf.Write(pktDelim)
	var shallowLines []string
	if depth != nil {
		for _, b := range c.boundary {
			shallowLines = append(shallowLines, "shallow "+b)
		}
		for _, s := range shallows {
			if c.includedCommits[s] && !containsStr(c.boundary, s) {
				shallowLines = append(shallowLines, "unshallow "+s)
			}
		}
	} else {
		for _, s := range shallows {
			shallowLines = append(shallowLines, "shallow "+s)
		}
	}
	if len(shallowLines) > 0 {
		buf.Write(pktLineStr("shallow-info\n"))
		for _, l := range shallowLines {
			buf.Write(pktLineStr(l + "\n"))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktLineStr("packfile\n"))
	// Sideband band 1: the pack, chunked to the 64k pkt-line limit.
	off := 0
	for off < len(pack) {
		end := off + 65515
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[off:end]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		buf.Write(pktLine(payload))
		off = end
	}
	if len(pack) == 0 {
		buf.Write(pktLine([]byte{1}))
	}
	buf.Write(pktFlush)
	return gitOK("application/x-git-upload-pack-result", buf.Bytes())
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func uploadPackV0(r *repo, body []byte) gitResponse {
	first, rest, err := parsePktLines(body)
	if err != nil {
		panic(err)
	}
	second, _, err := parsePktLines(rest)
	if err != nil {
		panic(err)
	}
	var wants, haves, shallows []string
	var depth *int
	for _, l := range first {
		switch {
		case strings.HasPrefix(l, "want "):
			wants = append(wants, strings.Split(l[5:], " ")[0])
		case strings.HasPrefix(l, "shallow "):
			shallows = append(shallows, strings.TrimSpace(l[8:]))
		case strings.HasPrefix(l, "deepen "):
			var d int
			fmt.Sscanf(strings.TrimSpace(l[7:]), "%d", &d)
			depth = &d
		}
	}
	for _, l := range second {
		if strings.HasPrefix(l, "have ") {
			haves = append(haves, strings.TrimSpace(l[5:]))
		}
	}
	c := closure(r, wants, haves, depth, shallows)
	pack := writePack(c.objects)
	var buf bytes.Buffer
	buf.Write(pktLineStr("NAK\n"))
	if depth != nil {
		for _, b := range c.boundary {
			buf.Write(pktLineStr("shallow " + b + "\n"))
		}
		for _, s := range shallows {
			if c.includedCommits[s] && !containsStr(c.boundary, s) {
				buf.Write(pktLineStr("unshallow " + s + "\n"))
			}
		}
	} else {
		for _, s := range shallows {
			buf.Write(pktLineStr("shallow " + s + "\n"))
		}
	}
	off := 0
	for off < len(pack) {
		end := off + 65515
		if end > len(pack) {
			end = len(pack)
		}
		chunk := pack[off:end]
		payload := make([]byte, 1+len(chunk))
		payload[0] = 1
		copy(payload[1:], chunk)
		buf.Write(pktLine(payload))
		off = end
	}
	buf.Write(pktFlush)
	return gitOK("application/x-git-upload-pack-result", buf.Bytes())
}

// ---------------------------------------------------------------- receive-pack (v0)

type pushReport struct {
	unpackOK bool
	refs     []pushRefReport
	sideband bool
}

type pushRefReport struct {
	ref    string
	ok     bool
	reason string
}

var pushCommandRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

// POST git-receive-pack: apply a push, answer report-status (sideband when negotiated).
func handleReceivePack(r *repo, body []byte) (out gitResponse) {
	if r == nil {
		return gitNotFound()
	}
	defer func() {
		if rec := recover(); rec != nil {
			var msg string
			if err, ok := rec.(error); ok {
				msg = err.Error()
			} else {
				msg = fmt.Sprint(rec)
			}
			var buf bytes.Buffer
			buf.Write(pktLineStr("unpack " + msg + "\n"))
			buf.Write(pktFlush)
			out = gitOK("application/x-git-receive-pack-result", buf.Bytes())
		}
	}()
	lines, rest, err := parsePktLines(body)
	if err != nil {
		panic(err)
	}
	type command struct {
		oldOID string
		newOID string
		ref    string
	}
	var commands []command
	caps := ""
	for _, line := range lines {
		nul := strings.Index(line, "\x00")
		text := line
		if nul >= 0 {
			text = line[:nul]
			caps = line[nul+1:]
		}
		m := pushCommandRe.FindStringSubmatch(text)
		if m != nil {
			commands = append(commands, command{oldOID: m[1], newOID: m[2], ref: m[3]})
		}
	}
	sideband := false
	for _, c := range strings.Split(caps, " ") {
		if c == "side-band-64k" {
			sideband = true
		}
	}
	report := pushReport{unpackOK: true, sideband: sideband}
	// The pack follows the flush. A delete-only push sends no pack.
	if len(rest) > 0 {
		entries, err := readPack(rest)
		if err != nil {
			panic(err)
		}
		for oid, entry := range entries {
			if _, ok := r.objects[oid]; !ok {
				r.objects[oid] = &gitObject{typ: entry.typ, data: entry.data}
				if entry.typ == objCommit {
					r.commitSeq[oid] = len(r.commitSeq) + 1
				}
			}
		}
	}
	for _, cmd := range commands {
		if cmd.newOID == zeroOID {
			// Delete.
			cur, ok := r.refs[cmd.ref]
			if !ok || cur != cmd.oldOID {
				report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(r.refs, cmd.ref)
				report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: true})
			}
			continue
		}
		cur, hasCur := r.refs[cmd.ref]
		if cmd.oldOID == zeroOID {
			if hasCur {
				report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
		} else if !hasCur || cur != cmd.oldOID {
			report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			continue
		} else if !isAncestor(r, cmd.oldOID, cmd.newOID) {
			report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		r.refs[cmd.ref] = cmd.newOID
		report.refs = append(report.refs, pushRefReport{ref: cmd.ref, ok: true})
	}
	var linesOut []string
	linesOut = append(linesOut, "unpack ok")
	for _, rep := range report.refs {
		if rep.ok {
			linesOut = append(linesOut, "ok "+rep.ref)
		} else {
			reason := rep.reason
			if reason == "" {
				reason = "failed"
			}
			linesOut = append(linesOut, "ng "+rep.ref+" "+reason)
		}
	}
	var buf bytes.Buffer
	if sideband {
		for _, l := range linesOut {
			payload := append([]byte{1}, []byte(l+"\n")...)
			buf.Write(pktLine(payload))
		}
	} else {
		for _, l := range linesOut {
			buf.Write(pktLineStr(l + "\n"))
		}
	}
	buf.Write(pktFlush)
	return gitOK("application/x-git-receive-pack-result", buf.Bytes())
}

// Create a commit on branch that sets path to content (or deletes it when content is nil).
func commitOnBranch(s *store, r *repo, branch, path string, content []byte, message string) (commitOID string, blobOID string) {
	ref := "refs/heads/" + branch
	tip, hasTip := r.refs[ref]
	var treeOID string
	var parents []string
	if hasTip {
		if obj := r.objects[tip]; obj != nil && obj.typ == objCommit {
			treeOID = parseCommit(obj.data).tree
			parents = append(parents, tip)
		}
	}
	blob := ""
	if content != nil {
		blob = storeObject(r, objBlob, content)
	}
	nextTree := treeSetPath(r, treeOID, path, blob)
	line, _ := identLine(s)
	commitData := buildCommit(nextTree, parents, line, message)
	commitOID = storeObject(r, objCommit, commitData)
	if _, ok := r.commitSeq[commitOID]; !ok {
		r.commitSeq[commitOID] = len(r.commitSeq) + 1
	}
	r.refs[ref] = commitOID
	return commitOID, blob
}
