package steps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GitLab returns a merge request description with every CRLF replaced by LF.
// glab sends the description unchanged, so the API does it (#1795). Evidence
// captured from HTTP exchanges routinely carries CRLF, so a body published as
// sent never read back as sent: the PR step refused the merge request it had
// just created, and the next run refused its appendix as edited, because the
// digest was stamped over the CRLF bytes.

var lineEndingsToLF = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// normalizePRNewlines makes a GitLab body a fixed point of that read. It also
// turns a lone CR into LF, so the result is stable whatever GitLab does with
// one. CommonMark reads CR, LF and CRLF alike as a line ending, so the
// rendering does not change. Other providers keep their bytes: GitHub returns
// CRLF as written. It runs at the publication boundary, before the appendix
// digest is stamped.
func normalizePRNewlines(provider scm.Provider, s string) string {
	if provider != scm.ProviderGitLab || !strings.Contains(s, "\r") {
		return s
	}
	return lineEndingsToLF.Replace(s)
}

// maxCRLFRecoveryBytes bounds the bytes crlfRestoredDigestMatches hashes. With
// SHA extensions that is well under a second; it is spent only on an appendix
// whose digest does not match as read.
const maxCRLFRecoveryBytes = 128 << 20

// crlfRestoredDigestMatches reports whether restoring CR before some line feeds
// of appendix reproduces digest: an appendix that an earlier version stamped
// over CRLF evidence, which GitLab then returned with LF. CR was only ever in
// captured output, so only content lines of fenced code blocks are candidates.
// Trying every subset is not feasible, so the search covers the shapes
// captured output has. Each block is CRLF throughout or LF throughout, except
// that at most one block has a single run of lines of the other kind (a
// multipart body inside a logged request, or a response body between two
// header blocks). Uniform shapes are tried first, then each block, smallest
// first, as the mixed one. A placement outside these shapes, or past the byte
// budget, still fails like an edit. The digest decides every candidate, so an
// edited appendix never matches.
func crlfRestoredDigestMatches(appendix, digest string) bool {
	if strings.Contains(appendix, "\r\n") {
		return false
	}
	want, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	// A FIPS 140 v1.0.0 build may not clone hash state; the search then fails
	// closed.
	start, ok := sha256.New().(hash.Cloner)
	if !ok {
		return false
	}
	text := []byte(appendix)
	search := &crlfSearch{text: text, want: want, blocks: fencedContentLineEnds(text)}
	if len(search.blocks) == 0 {
		return false
	}
	if search.walk(start, 0, 0, -1) {
		return true
	}
	order := make([]int, len(search.blocks))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return len(search.blocks[a]) - len(search.blocks[b]) })
	for _, mixed := range order {
		if search.walk(start, 0, 0, mixed) {
			return true
		}
		if search.exhausted() {
			return false
		}
	}
	return false
}

// fencedContentLineEnds returns, for each fenced code block of text with
// content, the offsets of the line feeds that end its content lines.
func fencedContentLineEnds(text []byte) [][]int {
	var blocks [][]int
	var block []int
	var fence markdownFence
	for start := 0; start <= len(text); {
		// The appendix ends without a line feed, often on a closing fence.
		end := len(text)
		if i := bytes.IndexByte(text[start:], '\n'); i >= 0 {
			end = start + i
		}
		inFence := fence.marker != 0
		fence.consume(string(text[start:end]))
		switch {
		case inFence && fence.marker == 0:
			if len(block) > 0 {
				blocks = append(blocks, block)
			}
			block = nil
		case inFence:
			// A block still open at the end is never appended.
			block = append(block, end)
		}
		start = end + 1
	}
	return blocks
}

type crlfSearch struct {
	text   []byte
	want   []byte
	blocks [][]int
	spent  int
}

func (s *crlfSearch) exhausted() bool { return s.spent > maxCRLFRecoveryBytes }

// lineRun is the half-open range of a block's content lines that get CR.
type lineRun struct{ lo, hi int }

var carriageReturn = []byte{'\r'}

// walk extends a clone of h, which has hashed text up to pos, with each shape
// of block b and recurses into the blocks after it. h itself is never written.
// Only block mixed takes a mixed shape.
func (s *crlfSearch) walk(h hash.Cloner, b, pos, mixed int) bool {
	if b == len(s.blocks) {
		s.spent += len(s.text) - pos
		clone, err := h.Clone()
		if err != nil {
			s.spent = maxCRLFRecoveryBytes + 1
			return false
		}
		clone.Write(s.text[pos:])
		return bytes.Equal(clone.Sum(nil), s.want)
	}
	ends := s.blocks[b]
	next := ends[len(ends)-1] + 1
	try := func(runs ...lineRun) bool {
		if s.exhausted() {
			return false
		}
		clone, err := h.Clone()
		if err != nil {
			s.spent = maxCRLFRecoveryBytes + 1
			return false
		}
		at := pos
		for _, run := range runs {
			for _, end := range ends[run.lo:run.hi] {
				clone.Write(s.text[at:end])
				clone.Write(carriageReturn)
				at = end
			}
		}
		clone.Write(s.text[at:next])
		s.spent += next - pos
		return s.walk(clone, b+1, next, mixed)
	}
	n := len(ends)
	if b != mixed {
		return try() || try(lineRun{0, n})
	}
	for lo := 0; lo < n; lo++ {
		for hi := lo + 1; hi <= n; hi++ {
			if lo == 0 && hi == n {
				continue
			}
			if try(lineRun{lo, hi}) || (lo > 0 && hi < n && try(lineRun{0, lo}, lineRun{hi, n})) {
				return true
			}
			if s.exhausted() {
				return false
			}
		}
	}
	return false
}
