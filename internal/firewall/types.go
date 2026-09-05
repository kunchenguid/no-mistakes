package firewall

// Class is one Hard Rules category. Values are stable API tokens for the
// LAN portal; they are not customer identifiers.
type Class string

const (
	ClassHostname   Class = "hostname"
	ClassSiteCode   Class = "site_code"
	ClassIPAddress  Class = "ip_address"
	ClassMACAddress Class = "mac_address"
	ClassSerial     Class = "serial"
	ClassFirmware   Class = "firmware_build"
	ClassGPS        Class = "gps"
	ClassTelephone  Class = "telephone"
	ClassIdentity   Class = "identity"
	ClassK8s        Class = "k8s_identifier"
	ClassPolicyName Class = "policy_name"
	ClassCapture    Class = "capture"
	ClassFleetScale Class = "fleet_scale"
	ClassFilename   Class = "capture_filename"
)

// Input is every surface the Hard Rules say counts: the PR diff (added and
// removed lines), filenames, title, commit messages, and body.
type Input struct {
	Diff           string
	Title          string
	CommitMessages []string
	Body           string
	Repo           string
	PRURL          string
	PRNumber       int
	HeadSHA        string
	BaseSHA        string
	Branch         string
}

// Finding is a LAN-private hit. Description may include a snippet.
type Finding struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Class       Class  `json:"class"`
	Action      string `json:"action"`
	Description string `json:"description"`
}

// Result is the full scan outcome.
type Result struct {
	Findings []Finding
	Error    string
}

func (r Result) Failed() bool {
	return r.Error != "" || len(r.Findings) > 0
}

func (r Result) Conclusion() string {
	if r.Error != "" {
		return "error"
	}
	if len(r.Findings) > 0 {
		return "failure"
	}
	return "success"
}

func (r Result) ClassCounts() map[string]int {
	counts := map[string]int{}
	for _, f := range r.Findings {
		counts[string(f.Class)]++
	}
	return counts
}

// PublicPhrase is the only violation sentence allowed on GitHub, Discord,
// commit messages, and PR titles produced by this package.
const PublicPhrase = "publish-policy violation"
