package firewall

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"net"
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
	var hits []Finding
	for _, s := range surfaces(in) {
		hits = append(hits, detect(s)...)
	}
	return Result{Findings: hits}
}

func surfaces(in Input) []surface {
	out := parseDiff(in.Diff)
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
	return out
}

func parseDiff(diff string) []surface {
	if strings.TrimSpace(diff) == "" {
		return nil
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
	return out
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
	hits = append(hits, detectIPs(s)...)
	hits = append(hits, detectMACs(s)...)
	hits = append(hits, detectEmails(s)...)
	hits = append(hits, detectPhones(s)...)
	hits = append(hits, detectHosts(s)...)
	hits = append(hits, detectSiteCodes(s)...)
	hits = append(hits, detectSerials(s)...)
	hits = append(hits, detectFirmware(s)...)
	hits = append(hits, detectGPS(s)...)
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

func detectIPs(s surface) []Finding {
	var hits []Finding
	for _, tok := range ipv4Candidate.FindAllString(s.text, -1) {
		ip, n, ok := parseIPv4Candidate(tok)
		if !ok {
			continue
		}
		if isDocIPv4(ip, n) {
			continue
		}
		hits = append(hits, hit(s, ClassIPAddress, tok))
	}
	for _, tok := range ipv6Loose.FindAllString(s.text, -1) {
		if strings.Count(tok, ":") < 2 {
			continue
		}
		ip := net.ParseIP(tok)
		if ip == nil || ip.To4() != nil {
			continue
		}
		if isDocIPv6(ip) {
			continue
		}
		hits = append(hits, hit(s, ClassIPAddress, tok))
	}
	return hits
}

func detectMACs(s surface) []Finding {
	var hits []Finding
	for _, tok := range macCandidate.FindAllString(s.text, -1) {
		if isDocMAC(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassMACAddress, tok))
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
		if isDocSite(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassSiteCode, tok))
	}
	return hits
}

func detectSerials(s surface) []Finding {
	var hits []Finding
	for _, tok := range serialKw.FindAllString(s.text, -1) {
		hits = append(hits, hit(s, ClassSerial, tok))
	}
	return hits
}

func detectFirmware(s surface) []Finding {
	var hits []Finding
	for _, tok := range firmwareKw.FindAllString(s.text, -1) {
		hits = append(hits, hit(s, ClassFirmware, tok))
	}
	return hits
}

func detectGPS(s surface) []Finding {
	if !gpsKw.MatchString(s.text) {
		return nil
	}
	var hits []Finding
	for _, tok := range gpsPair.FindAllString(s.text, -1) {
		if isDocGPS(tok) {
			continue
		}
		hits = append(hits, hit(s, ClassGPS, tok))
	}
	return hits
}

func detectK8s(s surface) []Finding {
	var hits []Finding
	for _, m := range k8sAssign.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 || isDocK8s(m[1]) {
			continue
		}
		hits = append(hits, hit(s, ClassK8s, m[0]))
	}
	return hits
}

func detectPolicy(s surface) []Finding {
	var hits []Finding
	for _, m := range policyKw.FindAllStringSubmatch(s.text, -1) {
		if len(m) < 2 {
			continue
		}
		name := strings.ToLower(m[1])
		if name == "example" || name == "test" || name == "demo" {
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
	for _, tok := range sessionKw.FindAllString(s.text, -1) {
		hits = append(hits, hit(s, ClassCapture, tok))
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
