package intent

import "regexp"

// filePathTokens picks out tokens that plausibly look like a file path.
// We require at least one alphanumeric char and allow common path chars.
var filePathTokens = regexp.MustCompile(`[A-Za-z0-9_./\\\-]+\.[A-Za-z0-9]{1,8}`)
