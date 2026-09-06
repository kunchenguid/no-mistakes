package firewall

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/oklog/ulid/v2"
)

type surface struct {
	kind string
	file string
	line int
	text string
}

var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

func newFindingID() string {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Now(), idEntropy).String()
}

// Scan inspects every counted surface. It never logs match text.
func Scan(in Input) Result {
	all, err := surfaces(in)
	if err != nil {
		return Result{Error: err.Error()}
	}
	var hits []Finding
	for _, s := range all {
		hits = append(hits, detect(s)...)
	}
	hits = append(hits, detectGPS(all)...)
	return Result{Findings: hits}
}

func surfaces(in Input) ([]surface, error) {
	out, err := parseDiff(in.Diff)
	if strings.TrimSpace(in.Title) != "" {
		out = append(out, surface{kind: "title", text: in.Title})
	}
	for _, msg := range in.CommitMessages {
		if strings.TrimSpace(msg) != "" {
			out = append(out, surface{kind: "commit", text: msg})
		}
	}
	if strings.TrimSpace(in.Body) != "" {
		out = append(out, surface{kind: "body", text: in.Body})
	}
	return out, err
}

func parseDiff(diff string) ([]surface, error) {
	if strings.TrimSpace(diff) == "" {
		return nil, nil
	}
	var out []surface
	file := ""
	plusLine := 0
	minusLine := 0
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"), line == "GIT binary patch":
			return nil, fmt.Errorf("diff contains unscannable binary content")
		case strings.HasPrefix(line, "diff --git "):
			for _, name := range diffFilenames(strings.TrimPrefix(line, "diff --git ")) {
				out = append(out, surface{kind: "filename", file: name, text: name})
			}
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "):
			name := strings.TrimPrefix(strings.TrimPrefix(line, "rename from "), "rename to ")
			if decoded, err := strconv.Unquote(name); err == nil {
				name = decoded
			}
			out = append(out, surface{kind: "filename", file: name, text: name})
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(line, "+++ ")
			file = strings.TrimPrefix(file, "b/")
			out = append(out, surface{kind: "filename", file: file, text: file})
		case strings.HasPrefix(line, "--- "):
			name := strings.TrimPrefix(line, "--- ")
			name = strings.TrimPrefix(name, "a/")
			out = append(out, surface{kind: "filename", file: name, text: name})
		case strings.HasPrefix(line, "@@ "):
			plusLine, minusLine = parseHunk(line)
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			out = append(out, surface{kind: "added", file: file, line: plusLine, text: line[1:]})
			plusLine++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			out = append(out, surface{kind: "removed", file: file, line: minusLine, text: line[1:]})
			minusLine++
		default:
			plusLine++
			minusLine++
		}
	}
	return out, sc.Err()
}

func diffFilenames(header string) []string {
	var left, right string
	if strings.HasPrefix(header, `"`) {
		for i := 1; i < len(header); i++ {
			if header[i] == '\\' {
				i++
			} else if header[i] == '"' {
				left, right = header[:i+1], strings.TrimSpace(header[i+1:])
				break
			}
		}
	} else {
		i := strings.LastIndex(header, " b/")
		if i < 0 {
			i = strings.LastIndex(header, ` "b/`)
		}
		if i >= 0 {
			left, right = header[:i], header[i+1:]
		}
	}
	var names []string
	for _, name := range []string{left, right} {
		if decoded, err := strconv.Unquote(name); err == nil {
			name = decoded
		}
		if len(name) >= 2 {
			names = append(names, name[2:])
		}
	}
	return names
}

func parseHunk(hunk string) (plus, minus int) {
	// @@ -l,s +l,s @@
	fields := strings.Fields(hunk)
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "-"):
			minus = hunkStart(f)
		case strings.HasPrefix(f, "+"):
			plus = hunkStart(f)
		}
	}
	return plus, minus
}

func hunkStart(f string) int {
	f = strings.TrimPrefix(f, "+")
	f = strings.TrimPrefix(f, "-")
	n, _, _ := strings.Cut(f, ",")
	var v int
	fmt.Sscanf(n, "%d", &v)
	return v
}

func detect(s surface) []Finding {
	var hits []Finding
	hits = append(hits, detectAddresses(s)...)
	hits = append(hits, detectEmails(s)...)
	hits = append(hits, detectPhones(s)...)
	hits = append(hits, detectHosts(s)...)
	hits = append(hits, detectSiteCodes(s)...)
	hits = append(hits, detectSerials(s)...)
	hits = append(hits, detectFirmware(s)...)
	hits = append(hits, detectK8s(s)...)
	hits = append(hits, detectPolicy(s)...)
	hits = append(hits, detectCaptures(s)...)
	hits = append(hits, detectFleet(s)...)
	return hits
}

func hit(s surface, class Class, snippet string) Finding {
	loc := s.kind
	if s.file != "" {
		loc = s.file
	}
	return Finding{
		ID:       newFindingID(),
		Severity: types.FindingSeverityError,
		File:     s.file,
		Line:     s.line,
		Class:    class,
		Action:   types.ActionAskUser,
		Description: fmt.Sprintf(
			"%s match on %s (line %d): %s",
			class, loc, s.line, clip(snippet, 80),
		),
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func detectAddresses(s surface) []Finding {
	var hits []Finding
	var ipv6Spans [][]int
	for _, loc := range ipv6Loose.FindAllStringIndex(s.text, -1) {
		tok := s.text[loc[0]:loc[1]]
		if strings.Count(tok, ":") < 2 {
			continue
		}
		var ip net.IP
		var network *net.IPNet
		if strings.Contains(tok, "/") {
			var err error
			ip, network, err = net.ParseCIDR(tok)
			if err != nil {
				continue
			}
		} else {
			ip = net.ParseIP(tok)
		}
		if ip == nil {
			continue
		}
		ipv6Spans = append(ipv6Spans, loc)
		if !isDocIPv6(ip, network) {
			hits = append(hits, hit(s, ClassIPAddress, tok))
		}
	}
	for _, loc := range ipv4Candidate.FindAllStringIndex(s.text, -1) {
		if overlapsMatch(loc, ipv6Spans) {
			continue
		}
		tok := s.text[loc[0]:loc[1]]
		ip, n, ok := parseIPv4Candidate(tok)
		if !ok || isDocIPv4(ip, n) {
			continue
		}
		hits = append(hits, hit(s, ClassIPAddress, tok))
	}
	for _, loc := range macCandidate.FindAllStringIndex(s.text, -1) {
		if overlapsMatch(loc, ipv6Spans) {
			continue
		}
		tok := s.text[loc[0]:loc[1]]
		if !isDocMAC(tok) {
			hits = append(hits, hit(s, ClassMACAddress, tok))
		}
	}
	return hits
}

func detectEmails(s surface) []Finding {
	var hits []Finding
	for _, tok := range emailCandidate.FindAllString(s.text, -1) {
		if isDocEmail(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassIdentity, tok))
	}
	return hits
}

func detectPhones(s surface) []Finding {
	var hits []Finding
	for _, tok := range phoneCandidate.FindAllString(s.text, -1) {
		if isDocPhone(tok) {
			continue
		}
		// Require at least one separator so unseparated IDs are not phones.
		if !strings.ContainsAny(tok, "-.() ") && !strings.Contains(tok, "+") {
			continue
		}
		hits = append(hits, hit(s, ClassTelephone, tok))
	}
	return hits
}

func detectHosts(s surface) []Finding {
	var hits []Finding
	for _, tok := range internalHost.FindAllString(s.text, -1) {
		if isDocHost(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassHostname, tok))
	}
	return hits
}

func detectSiteCodes(s surface) []Finding {
	var hits []Finding
	for _, tok := range siteCode.FindAllString(s.text, -1) {
		if !isSiteCode(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassSiteCode, tok))
	}
	return hits
}

func detectSerials(s surface) []Finding {
	var hits []Finding
	for _, m := range serialKw.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || !isCapturedLiteral(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassSerial, m[0]))
	}
	return hits
}

func detectFirmware(s surface) []Finding {
	var hits []Finding
	for _, m := range firmwareKw.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || !isVersionShaped(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassFirmware, m[0]))
	}
	return hits
}

// detectGPS reads the whole surface list because a coordinate pair is
// routinely serialized one component per line. A keyword assigned a single
// coordinate is a hit on its own line; a bare pair needs a keyword on its own
// or an adjacent diff line.
func detectGPS(all []surface) []Finding {
	var hits []Finding
	for i := range all {
		s := all[i]
		assigned := gpsAssign.FindAllStringSubmatchIndex(s.text, -1)
		for _, m := range assigned {
			if isDocCoordinate(s.text[m[2]:m[3]]) {
				continue
			}
			hits = append(hits, hit(s, ClassGPS, s.text[m[0]:m[1]]))
		}
		if !gpsKeywordNear(all, i) {
			continue
		}
		for _, loc := range gpsPair.FindAllStringIndex(s.text, -1) {
			if overlapsMatch(loc, assigned) {
				continue
			}
			tok := s.text[loc[0]:loc[1]]
			if isDocGPS(tok) {
				continue
			}
			hits = append(hits, hit(s, ClassGPS, tok))
		}
	}
	return hits
}

func gpsKeywordNear(all []surface, i int) bool {
	if gpsKw.MatchString(all[i].text) {
		return true
	}
	for _, j := range []int{i - 1, i + 1} {
		if j < 0 || j >= len(all) {
			continue
		}
		if adjacentContent(all[i], all[j]) && gpsKw.MatchString(all[j].text) {
			return true
		}
	}
	return false
}

func adjacentContent(a, b surface) bool {
	return a.file == b.file && a.kind == b.kind && isContentKind(a.kind) &&
		(a.line == b.line+1 || b.line == a.line+1)
}

func isContentKind(kind string) bool {
	return kind == "added" || kind == "removed"
}

func overlapsMatch(loc []int, matches [][]int) bool {
	i := sort.Search(len(matches), func(i int) bool { return matches[i][1] > loc[0] })
	return i < len(matches) && matches[i][0] < loc[1]
}

func detectK8s(s surface) []Finding {
	var hits []Finding
	for _, m := range k8sAssign.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || isDocK8s(m[1]) || !isLiveShaped(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassK8s, m[0]))
	}
	return hits
}

func detectPolicy(s surface) []Finding {
	var hits []Finding
	for _, m := range policyKw.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || !isLiveShaped(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassPolicyName, m[0]))
	}
	return hits
}

func detectCaptures(s surface) []Finding {
	var hits []Finding
	if s.kind == "filename" && captureName.MatchString(s.text) {
		hits = append(hits, hit(s, ClassFilename, s.text))
	}
	for _, m := range sessionKw.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || !isCapturedLiteral(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassCapture, m[0]))
	}
	if syslogLine.MatchString(s.text) {
		hits = append(hits, hit(s, ClassCapture, clip(s.text, 40)))
	}
	return hits
}

func detectFleet(s surface) []Finding {
	var hits []Finding
	for _, tok := range fleetScale.FindAllString(s.text, -1) {
		hits = append(hits, hit(s, ClassFleetScale, tok))
	}
	return hits
}
