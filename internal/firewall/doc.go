// Package firewall scans proposed git publications against the Hard Rules
// classes: never commit data captured from a live system, and never let a
// real value become a fixture.
//
// Detectors are structural and anchored. There is no organization-name or
// abbreviation dictionary: a bare three-letter match is ordinary English
// (`equal`, `manual`, `actual`). Documentation ranges are allowlisted
// (RFC 5737 TEST-NET, RFC 3849, RFC 2606 example domains, IANA documentation
// MACs, 555-01xx). Everything else in a high-confidence class fails closed.
//
// Public GitHub / Discord text is owned by PublicSummary and Notice: those
// helpers must never copy a match snippet, filename, hostname, address, or
// person name into a public surface. LAN findings keep the location and
// snippet so an operator can fix the diff.
package firewall
