package firewall

import (
	"strings"
	"testing"
)

func TestScan_DocumentationRangesAreAllowed(t *testing.T) {
	diff := unified("docs/example.md", []string{
		"host01.example.com",
		"user@example.com",
		"192.0.2.1",
		"198.51.100.10",
		"203.0.113.5",
		"2001:db8::1",
		"127.0.0.1",
		"00:00:5e:00:53:01",
		"555-0100",
		"SITE01",
		"lat 0.0, 0.0",
		"namespace: default",
	})
	res := Scan(Input{Diff: diff, Title: "docs: add example values"})
	if len(res.Findings) != 0 {
		t.Fatalf("documentation values flagged: %+v", classes(res))
	}
}

func TestScan_FlagsNonDocumentationClasses(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		class Class
	}{
		{"rfc1918", "bind 10.0.0.5", ClassIPAddress},
		{"public-ipv4", "resolver 8.8.8.8", ClassIPAddress},
		{"mac", "hwaddr 02:00:00:00:00:01", ClassMACAddress},
		{"email", "contact noc@lab.internal", ClassIdentity},
		{"phone", "call 212-555-1234", ClassTelephone},
		{"host", "ssh switch01.core.internal", ClassHostname},
		{"site", "facility-west12", ClassSiteCode},
		{"serial", "serial: SN9F3K21AB", ClassSerial},
		{"gps", "gps 12.345678, 98.765432", ClassGPS},
		{"k8s", "namespace: prod-tenant-a", ClassK8s},
		{"ssid", "ssid: corp-guest", ClassPolicyName},
		{"session", "session_id=abcdef1234567890", ClassCapture},
		{"fleet", "deployed 12000 devices", ClassFleetScale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Scan(Input{Diff: unified("cfg.txt", []string{tc.line})})
			if !hasClass(res, tc.class) {
				t.Fatalf("want class %s, got %+v for %q", tc.class, classes(res), tc.line)
			}
		})
	}
}

func TestScan_OrdinaryEnglishIsNotAnOrgHit(t *testing.T) {
	diff := unified("english.go", []string{
		"equal",
		"manual",
		"actual",
		"virtual",
		"toEqual",
		"quality",
	})
	res := Scan(Input{Diff: diff, Title: "test: equal manual actual quality"})
	if len(res.Findings) != 0 {
		t.Fatalf("ordinary English matched as policy: %+v", res.Findings)
	}
}

func TestScan_RemovedLineStillFails(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/cfg.txt b/cfg.txt",
		"--- a/cfg.txt",
		"+++ b/cfg.txt",
		"@@ -1,1 +1,0 @@",
		"-bind 10.0.0.5",
	}, "\n") + "\n"
	res := Scan(Input{Diff: diff})
	if !hasClass(res, ClassIPAddress) {
		t.Fatalf("deleted live IP must still fail, got %+v", classes(res))
	}
}

func TestScan_TitleAndCommitCount(t *testing.T) {
	res := Scan(Input{
		Diff:           unified("readme.md", []string{"no addresses here"}),
		Title:          "fix 10.0.0.5 leak",
		CommitMessages: []string{"drop noc@lab.internal from fixture"},
	})
	if !hasClass(res, ClassIPAddress) {
		t.Fatalf("title IP not detected: %+v", classes(res))
	}
	if !hasClass(res, ClassIdentity) {
		t.Fatalf("commit email not detected: %+v", classes(res))
	}
}

func TestScan_CaptureFilename(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/trace.pcap b/trace.pcap",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/trace.pcap",
		"@@ -0,0 +1 @@",
		"+placeholder",
	}, "\n") + "\n"
	res := Scan(Input{Diff: diff})
	if !hasClass(res, ClassFilename) {
		t.Fatalf("pcap filename not flagged: %+v", classes(res))
	}
}

func TestPublicText_OmitsSnippets(t *testing.T) {
	res := Scan(Input{Diff: unified("cfg.txt", []string{"bind 10.0.0.5"})})
	if len(res.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	text := PublicText("http://portal.lan/v1/axi/runs/01", true)
	if !strings.Contains(text, PublicPhrase) {
		t.Fatalf("missing generic phrase: %q", text)
	}
	if containsForbiddenPublic(text, res.Findings) {
		t.Fatalf("public text leaked a match: %q findings=%+v", text, res.Findings)
	}
	if strings.Contains(text, "10.0.0.5") {
		t.Fatal("public text echoed the address")
	}
	if strings.Contains(text, "cfg.txt") {
		t.Fatal("public text echoed the filename")
	}
}

func TestNotice_OmitsSnippets(t *testing.T) {
	res := Scan(Input{Diff: unified("cfg.txt", []string{"bind 10.0.0.5"})})
	n := NewNotice("carverauto/serviceradar", "https://github.com/carverauto/serviceradar/pull/1", "http://portal.lan/v1/axi/runs/01", res.Conclusion())
	blob := n.Kind + n.Repo + n.PRURL + n.PortalURL + n.Conclusion
	if containsForbiddenPublic(blob, res.Findings) {
		t.Fatalf("notice leaked: %+v", n)
	}
	if n.Kind != "publish-policy-violation" {
		t.Fatalf("kind=%s", n.Kind)
	}
}

// containsForbiddenPublic reports whether s includes a match snippet from
// findings. It proves GitHub / notice output stays generic.
func containsForbiddenPublic(s string, findings []Finding) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "@") && strings.Contains(lower, ".") {
		// Email-shaped text is never allowed on a public surface.
		if emailCandidate.FindString(s) != "" {
			return true
		}
	}
	for _, f := range findings {
		if f.File != "" && strings.Contains(s, f.File) {
			return true
		}
		if f.Description != "" {
			// Description is LAN-private; any copy into public text is a leak.
			snip := f.Description
			if i := strings.LastIndex(snip, ": "); i >= 0 {
				snip = snip[i+2:]
			}
			if snip != "" && strings.Contains(s, snip) {
				return true
			}
		}
	}
	return ipv4Candidate.FindString(s) != "" || macCandidate.FindString(s) != ""
}

func TestScan_GPSSplitAcrossLines(t *testing.T) {
	res := Scan(Input{Diff: unified("site.json", []string{
		`  "site": {`,
		`    "latitude": 37.774929,`,
		`    "longitude": -122.419416`,
		"  }",
	})})
	if !hasClass(res, ClassGPS) {
		t.Fatalf("multi-line coordinates not flagged: %+v", classes(res))
	}
}

func TestScan_BarePairNeedsAnAdjacentKeyword(t *testing.T) {
	near := Scan(Input{Diff: unified("site.csv", []string{
		"# gps fix",
		"37.774929, -122.419416",
	})})
	if !hasClass(near, ClassGPS) {
		t.Fatalf("pair under a gps keyword not flagged: %+v", classes(near))
	}
	far := Scan(Input{Diff: unified("bench.txt", []string{
		"p99 latency budget",
		"37.774929, -122.419416",
	})})
	if hasClass(far, ClassGPS) {
		t.Fatalf("pair with no gps keyword flagged: %+v", classes(far))
	}
}

func TestScan_UnpairedOrdinaryFloatsAreNotGPS(t *testing.T) {
	res := Scan(Input{Diff: unified("bench.go", []string{
		"timeout := 30.500",
		"ratio = 1.2345",
		"threshold: 99.999",
	})})
	if hasClass(res, ClassGPS) {
		t.Fatalf("ordinary floats flagged as coordinates: %+v", classes(res))
	}
}

func TestScan_OrdinaryConfigValuesAreNotLiveIdentifiers(t *testing.T) {
	res := Scan(Input{Diff: unified("deploy/portal.yaml", []string{
		"  namespace: no-mistakes",
		"  cluster: staging",
		"  build: true",
		"  vlan: 100",
		"  firewall: enabled",
		"Namespace: metav1.ObjectMeta{Namespace: srQL}",
	})})
	if len(res.Findings) != 0 {
		t.Fatalf("ordinary manifest and Go values flagged: %+v", res.Findings)
	}
}

func TestScan_LiveShapedIdentifiersStillFail(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		class Class
	}{
		{"tenant-compound", "namespace: prod-tenant-a", ClassK8s},
		{"instance-number", "cluster: eks-cluster07", ClassK8s},
		{"ssid", "ssid: corp-guest", ClassPolicyName},
		{"firmware-version", "firmware: 17.9.4a", ClassFirmware},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Scan(Input{Diff: unified("cfg.txt", []string{tc.line})})
			if !hasClass(res, tc.class) {
				t.Fatalf("want %s for %q, got %+v", tc.class, tc.line, classes(res))
			}
		})
	}
}

func TestScan_TracingAndSerialPlumbingIsNotACapture(t *testing.T) {
	res := Scan(Input{Diff: unified("internal/trace/trace.go", []string{
		"trace_id: req.TraceID",
		"sessionID: cookieValue",
		"x-request-id: uuid.NewString()",
		"serialNumber: row.SerialNumber",
		"Serial: device.Serial",
	})})
	if len(res.Findings) != 0 {
		t.Fatalf("code identifiers flagged as captured values: %+v", res.Findings)
	}
}

func TestScan_PortCountProseIsNotFleetScale(t *testing.T) {
	res := Scan(Input{Diff: unified("docs/scanner.md", []string{
		"scans up to 65535 ports per host",
		"probe 1024 ports",
	})})
	if hasClass(res, ClassFleetScale) {
		t.Fatalf("port-count prose flagged as fleet scale: %+v", res.Findings)
	}
}

func TestScan_RealCapturedValuesStillFail(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		class Class
	}{
		{"session", "session_id=abcdef1234567890", ClassCapture},
		{"trace", "trace_id: 4bf92f3577b34da6a3ce929d0e0e4736", ClassCapture},
		{"serial", "serial: SN9F3K21AB", ClassSerial},
		{"fleet-devices", "deployed 12000 devices", ClassFleetScale},
		{"fleet-sites", "monitors 4200 sites", ClassFleetScale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Scan(Input{Diff: unified("cfg.txt", []string{tc.line})})
			if !hasClass(res, tc.class) {
				t.Fatalf("want %s for %q, got %+v", tc.class, tc.line, classes(res))
			}
		})
	}
}

func TestScan_OpaqueTokensWithoutDigitsStillFail(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		class Class
	}{
		{"upper-session", "jsessionid=ABCDEFGHIJKLMNOP", ClassCapture},
		{"upper-session-header", "Session-Id: AAAABBBBCCCCDDDD", ClassCapture},
		{"upper-serial", "serial: ABCDEFGH", ClassSerial},
		{"hex-asset-tag", "asset tag: DEADBEEF", ClassSerial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Scan(Input{Diff: unified("fixture.txt", []string{tc.line})})
			if !hasClass(res, tc.class) {
				t.Fatalf("want %s for %q, got %+v", tc.class, tc.line, classes(res))
			}
		})
	}
}

// TestScan_SchemaFieldNamesAreNotSiteCodes pins the gate on the site class:
// the keyword prefix alone is not a code, so a product schema that carries
// site/facility columns does not turn a required check permanently red.
func TestScan_SchemaFieldNamesAreNotSiteCodes(t *testing.T) {
	res := Scan(Input{Diff: unified("internal/db/schema.sql", []string{
		"  site_id TEXT NOT NULL,",
		"  site_name TEXT,",
		"  facility_id INTEGER,",
		"  dc_name TEXT,",
		`ClassSiteCode Class = "site_code"`,
		"venv/lib/python3.12/site-packages",
		"<div class=\"site-header\">",
	})})
	if hasClass(res, ClassSiteCode) {
		t.Fatalf("ordinary schema and package identifiers flagged as site codes: %+v", res.Findings)
	}
}

func TestScan_NumberedSiteCodesStillFail(t *testing.T) {
	for _, line := range []string{"facility-west12", "dc-east-01", "site_north7", "datacenter-02", "airport-lhr", "dc-ashburn", "facility-northgate", "site-frankfurt"} {
		t.Run(line, func(t *testing.T) {
			res := Scan(Input{Diff: unified("inventory.yaml", []string{"  location: " + line})})
			if !hasClass(res, ClassSiteCode) {
				t.Fatalf("numbered site code %q not flagged: %+v", line, classes(res))
			}
		})
	}
}

func unified(file string, added []string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + file + " b/" + file + "\n")
	b.WriteString("--- a/" + file + "\n")
	b.WriteString("+++ b/" + file + "\n")
	b.WriteString("@@ -0,0 +1," + itoa(len(added)) + " @@\n")
	for _, line := range added {
		b.WriteString("+" + line + "\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func hasClass(res Result, class Class) bool {
	for _, f := range res.Findings {
		if f.Class == class {
			return true
		}
	}
	return false
}

func classes(res Result) []Class {
	out := make([]Class, 0, len(res.Findings))
	for _, f := range res.Findings {
		out = append(out, f.Class)
	}
	return out
}

func TestScan_CompressedIPv6(t *testing.T) {
	for _, value := range []string{"fe80::1", "fd12:3456::7", "::abcd", "2001:4860:4860::8888"} {
		if res := Scan(Input{Diff: unified("config.txt", []string{"bind " + value})}); !hasClass(res, ClassIPAddress) {
			t.Fatalf("missed compressed address %q", value)
		}
	}
	for _, value := range []string{"::1", "::", "2001:db8::1234"} {
		if res := Scan(Input{Title: value}); hasClass(res, ClassIPAddress) {
			t.Fatalf("documentation address rejected: %q", value)
		}
	}
}

func TestScan_QuotedAssignments(t *testing.T) {
	for _, tc := range []struct {
		text  string
		class Class
	}{
		{`{"namespace":"prod-tenant-a"}`, ClassK8s},
		{`{"serial":"SN9F3K21AB"}`, ClassSerial},
		{`{"firmware":"17.9.4a"}`, ClassFirmware},
		{`{"ssid":"prod-network-a"}`, ClassPolicyName},
		{`{"session_id":"abcdef1234567890"}`, ClassCapture},
	} {
		if res := Scan(Input{Diff: unified("config.json", []string{tc.text})}); !hasClass(res, tc.class) {
			t.Fatalf("missed quoted assignment: %s", tc.text)
		}
	}
}

func TestScan_BinaryAndRenameMetadata(t *testing.T) {
	for _, diff := range []string{
		"diff --git a/trace.pcap b/trace.pcap\nnew file mode 100644\nBinary files /dev/null and b/trace.pcap differ\n",
		"diff --git a/trace.pcap b/trace.txt\nsimilarity index 100%\nrename from trace.pcap\nrename to trace.txt\n",
		"diff --git a/trace.txt b/trace.pcap\nsimilarity index 100%\nrename from trace.txt\nrename to trace.pcap\n",
		"diff --git a/packet trace.pcap b/packet trace.pcap\nBinary files /dev/null and b/packet trace.pcap differ\n",
		`diff --git "a/packet\ttrace.pcap" "b/packet\ttrace.pcap"` + "\n",
	} {
		if res := Scan(Input{Diff: diff}); !hasClass(res, ClassFilename) && res.Error == "" {
			t.Fatalf("capture metadata passed: %q", diff)
		}
	}
}

func TestGitHubCheck_OversizedDiffFailsAsError(t *testing.T) {
	in := Input{Diff: unified("large.txt", []string{strings.Repeat("x", 4<<20), "bind 10.0.0.5"})}
	res := Scan(in)
	if res.Error == "" || res.Conclusion() != "error" {
		t.Fatalf("partial scan certified: %+v", res)
	}
	out, code, err := GitHubCheck(in, CheckOptions{})
	if code != ExitError || err == nil || out != PublicErrorText("") {
		t.Fatalf("out=%q code=%d err=%v", out, code, err)
	}
}

func TestScan_CIDRRequiresCompleteAllowlistContainment(t *testing.T) {
	for _, tc := range []struct {
		value   string
		blocked bool
	}{
		{"192.0.3.7/23", true},
		{"192.0.2.7/23", true},
		{"198.51.100.7/22", true},
		{"203.0.113.7/23", true},
		{"0.0.0.0/0", true},
		{"127.0.0.1/7", true},
		{"192.0.2.0/24", false},
		{"192.0.2.7/25", false},
		{"192.0.2.7/32", false},
		{"198.51.100.7/24", false},
		{"203.0.113.7/24", false},
		{"0.0.0.0/32", false},
		{"127.0.0.1/8", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			res := Scan(Input{Diff: unified("config.txt", []string{"network: " + tc.value})})
			if got := hasClass(res, ClassIPAddress); got != tc.blocked {
				t.Fatalf("blocked=%v, want %v", got, tc.blocked)
			}
		})
	}
}

func TestScan_IPv6CIDRContainment(t *testing.T) {
	for _, tc := range []struct {
		value   string
		blocked bool
	}{
		{"::/0", true}, {"::1/127", true}, {"2001:db8::1/31", true},
		{"2001:db9::1/31", true}, {"fe80::1/64", true},
		{"::/128", false}, {"::1/128", false}, {"2001:db8::/32", false}, {"2001:db8::1/64", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			res := Scan(Input{Title: "network: " + tc.value})
			if got := hasClass(res, ClassIPAddress); got != tc.blocked {
				t.Fatalf("blocked=%v want=%v", got, tc.blocked)
			}
		})
	}
}

func TestScan_GPSContextRequiresAdjacentLinesOnSameSide(t *testing.T) {
	for _, tc := range []struct {
		diff    string
		blocked bool
	}{
		{"@@ -0,0 +1,2 @@\n+gps measurement\n+12.345, 67.890\n", true},
		{"@@ -1,2 +0,0 @@\n-gps measurement\n-12.345, 67.890\n", true},
		{"@@ -0,0 +1 @@\n+gps measurement\n@@ -200,0 +202 @@\n+12.345, 67.890\n", false},
		{"@@ -1 +1 @@\n-gps measurement\n+12.345, 67.890\n", false},
		{"@@ -1,1 +1,3 @@\n+gps measurement\n unchanged\n+12.345, 67.890\n", false},
	} {
		res := Scan(Input{Diff: "--- a/data.txt\n+++ b/data.txt\n" + tc.diff})
		if got := hasClass(res, ClassGPS); got != tc.blocked {
			t.Fatalf("blocked=%v want=%v diff=%q", got, tc.blocked, tc.diff)
		}
	}
}

func TestScan_IPv4MappedIPv6UsesIPv4Allowlist(t *testing.T) {
	for _, tc := range []struct {
		value   string
		blocked bool
	}{
		{"::ffff:a00:5", true}, {"::ffff:a00:5/128", true},
		{"::ffff:c000:307/119", true}, {"::ffff:c000:207/119", true},
		{"::ffff:c000:207/95", true}, {"::ffff:0:0/96", true},
		{"::ffff:c000:207", false}, {"::ffff:c000:207/120", false},
		{"::ffff:c000:207/121", false}, {"::ffff:c000:207/128", false},
		{"::ffff:7f00:1", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			in := Input{Title: "bind " + tc.value}
			if got := hasClass(Scan(in), ClassIPAddress); got != tc.blocked {
				t.Fatalf("blocked=%v want=%v", got, tc.blocked)
			}
			out, code, err := GitHubCheck(in, CheckOptions{})
			wantCode := 0
			if tc.blocked {
				wantCode = ExitViolation
			}
			if code != wantCode || err != nil || out != PublicText("", tc.blocked) {
				t.Fatalf("out=%q code=%d err=%v", out, code, err)
			}
		})
	}
}
