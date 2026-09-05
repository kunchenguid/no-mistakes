package firewall

import (
	"net"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

var (
	ipv4Candidate = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?:/\d{1,2})?\b`)
	// Word-bounded hex pairs. Deliberately requires separators so it does not
	// match unseparated SHA prefixes.
	macCandidate   = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b`)
	emailCandidate = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	// NANP with optional country code. 555-01xx is allowlisted in isDocPhone.
	phoneCandidate = regexp.MustCompile(`(?i)(?:\+?1[\s.\-]?)?(?:\(?\d{3}\)?[\s.\-]?)\d{3}[\s.\-]?\d{4}\b`)
	internalHost   = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:internal|corp|lan|intra|private|localdomain)\b`)
	// Site/facility labels with a delimiter. SITE01 is the documented
	// placeholder and is allowlisted. Three-letter tokens without a delimiter
	// are not matched (the abbreviation trap).
	siteCode    = regexp.MustCompile(`(?i)\b(?:site|facility|datacenter|airport|dc)[-_][a-z0-9]{2,}\b`)
	serialKw    = regexp.MustCompile(`(?i)\b(?:serial(?:\s*number)?|s/n|asset\s*tag|chassis\s*id)\s*[:=]\s*[A-Za-z0-9][A-Za-z0-9._:-]{3,}\b`)
	firmwareKw  = regexp.MustCompile(`(?i)\b(?:firmware|build(?:\s*number)?)\s*[:=]\s*[A-Za-z0-9][A-Za-z0-9._+-]{2,}\b`)
	gpsKw       = regexp.MustCompile(`(?i)\b(?:gps|lat(?:itude)?|lon(?:gitude)?|coord(?:inates)?)\b`)
	gpsPair     = regexp.MustCompile(`-?\d{1,3}\.\d{3,},\s*-?\d{1,3}\.\d{3,}`)
	k8sAssign   = regexp.MustCompile(`(?i)\b(?:namespace|cluster(?:\s*name)?|tenant(?:\s*id)?|workspace)\s*[:=]\s*["']?([A-Za-z0-9][A-Za-z0-9._:-]{1,})\b`)
	policyKw    = regexp.MustCompile(`(?i)\b(?:ssid|vlan|radius(?:\s*policy)?|802\.1x|aaa\s*policy|firewall(?:\s*rule)?)\s*[:=]\s*["']?([A-Za-z0-9][A-Za-z0-9._:-]{1,})\b`)
	sessionKw   = regexp.MustCompile(`(?i)\b(?:session[_-]?id|jsessionid|connect\.sid|trace[_-]?id|x-request-id)\s*[:=]\s*["']?[A-Za-z0-9._-]{8,}`)
	syslogLine  = regexp.MustCompile(`(?i)\b(?:[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2})\s+\S+\s+\S+(?:\[\d+\])?:`)
	fleetScale  = regexp.MustCompile(`(?i)\b\d{3,}(?:,\d{3})*\s+(?:devices?|sites?|ports?|endpoints?)\b`)
	captureName = regexp.MustCompile(`(?i)\.(?:pcap|pcapng|cap|dmp)$`)
	ipv6Loose   = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}\b`)
)

var docIPv4 = mustCIDRs(
	"192.0.2.0/24",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"127.0.0.0/8",
	"0.0.0.0/32",
	"255.255.255.255/32",
)

var docIPv6 = mustCIDRs(
	"::1/128",
	"::/128",
	"2001:db8::/32",
)

var docEmailDomains = map[string]struct{}{
	"example.com": {},
	"example.net": {},
	"example.org": {},
	"example.edu": {},
	"localhost":   {},
}

var docEmailTLDs = map[string]struct{}{
	"test":      {},
	"example":   {},
	"invalid":   {},
	"localhost": {},
}

var docK8sNames = map[string]struct{}{
	"default":         {},
	"kube-system":     {},
	"kube-public":     {},
	"kube-node-lease": {},
	"demo":            {},
	"example":         {},
	"test":            {},
	"local":           {},
	"cluster.local":   {},
}

var docSiteTokens = map[string]struct{}{
	"site01":     {},
	"site-01":    {},
	"facility01": {},
	"dc01":       {},
	"example":    {},
	"test":       {},
}

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func inCIDRs(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseIPv4Candidate(tok string) (net.IP, *net.IPNet, bool) {
	addr, mask, ok := strings.Cut(tok, "/")
	parts := strings.Split(addr, ".")
	if len(parts) != 4 {
		return nil, nil, false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil, nil, false
		}
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, nil, false
	}
	ip = ip.To4()
	if ip == nil {
		return nil, nil, false
	}
	if !ok {
		return ip, nil, true
	}
	bits, err := strconv.Atoi(mask)
	if err != nil || bits < 0 || bits > 32 {
		return nil, nil, false
	}
	_, n, err := net.ParseCIDR(tok)
	if err != nil {
		return nil, nil, false
	}
	return ip, n, true
}

func isDocIPv4(ip net.IP, n *net.IPNet) bool {
	if n != nil {
		ones, bits := n.Mask.Size()
		if bits == 32 && ones < 32 {
			// A published CIDR is a network plan. Only documentation
			// prefixes themselves are allowed.
			return inCIDRs(n.IP, docIPv4)
		}
	}
	return inCIDRs(ip, docIPv4)
}

func isDocIPv6(ip net.IP) bool {
	return inCIDRs(ip, docIPv6)
}

func isDocMAC(s string) bool {
	norm := strings.ToLower(strings.NewReplacer("-", ":", ".", ":").Replace(s))
	parts := strings.Split(norm, ":")
	if len(parts) != 6 {
		return false
	}
	// IANA documentation range 00:00:5e:00:53:00/24 (RFC 7042).
	return parts[0] == "00" && parts[1] == "00" && parts[2] == "5e" && parts[3] == "00" && parts[4] == "53"
}

func isDocEmail(addr string) bool {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return false
	}
	_, domain, ok := strings.Cut(parsed.Address, "@")
	if !ok {
		return false
	}
	domain = strings.ToLower(domain)
	if _, ok := docEmailDomains[domain]; ok {
		return true
	}
	if i := strings.LastIndex(domain, "."); i >= 0 {
		if _, ok := docEmailTLDs[domain[i+1:]]; ok {
			return true
		}
	}
	return false
}

func isDocPhone(s string) bool {
	digits := make([]rune, 0, 11)
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) == 11 && digits[0] == '1' {
		digits = digits[1:]
	}
	if len(digits) != 10 {
		return false
	}
	// 555-0100 through 555-0199.
	return string(digits[3:6]) == "555" && digits[6] == '0' && digits[7] == '1'
}

func isDocGPS(pair string) bool {
	pair = strings.ReplaceAll(pair, " ", "")
	a, b, ok := strings.Cut(pair, ",")
	if !ok {
		return false
	}
	lat, err1 := strconv.ParseFloat(a, 64)
	lon, err2 := strconv.ParseFloat(b, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return lat == 0 && lon == 0
}

func isDocSite(tok string) bool {
	_, ok := docSiteTokens[strings.ToLower(tok)]
	return ok
}

func isDocK8s(name string) bool {
	_, ok := docK8sNames[strings.ToLower(name)]
	return ok
}

func isDocHost(host string) bool {
	h := strings.ToLower(host)
	return strings.HasSuffix(h, ".example.com") ||
		strings.HasSuffix(h, ".example.net") ||
		strings.HasSuffix(h, ".example.org") ||
		h == "host01.example.com" ||
		h == "localhost"
}
