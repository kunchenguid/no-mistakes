package git

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Command-scope configuration outranks the run's worktree configuration. Remove
// only identity settings so agents can still create repositories with their own
// identities and keep unrelated command-scope settings, including credentials.
func stripIdentityConfig(overlay *runenv.Overlay, base []string) error {
	if base == nil {
		base = os.Environ()
	}
	values := make(map[string]string)
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		values[key] = value
	}
	set := func(key, value string) {
		if overlay.Set == nil {
			overlay.Set = make(map[string]string)
		}
		overlay.Set[key] = value
	}
	if raw, exists := values["GIT_CONFIG_COUNT"]; exists {
		var count int64
		var err error
		if raw != "" {
			count, err = strconv.ParseInt(strings.TrimLeft(raw, " \t\r\n\v\f"), 10, 64)
		}
		if err != nil || count < 0 || count > 2147483647 || count > int64(len(values)) {
			return fmt.Errorf("cannot isolate commit identity: invalid inherited GIT_CONFIG_COUNT")
		}
		retained := 0
		for n := int64(0); n < count; n++ {
			suffix := strconv.FormatInt(n, 10)
			key, keyOK := values["GIT_CONFIG_KEY_"+suffix]
			value, valueOK := values["GIT_CONFIG_VALUE_"+suffix]
			if !keyOK || !valueOK {
				return fmt.Errorf("cannot isolate commit identity: incomplete inherited Git config entry %d", n)
			}
			overlay.Unset = append(overlay.Unset, "GIT_CONFIG_KEY_"+suffix, "GIT_CONFIG_VALUE_"+suffix)
			if identityConfigKey(key) {
				continue
			}
			set("GIT_CONFIG_KEY_"+strconv.Itoa(retained), key)
			set("GIT_CONFIG_VALUE_"+strconv.Itoa(retained), value)
			retained++
		}
		set("GIT_CONFIG_COUNT", strconv.Itoa(retained))
	}
	if raw, exists := values["GIT_CONFIG_PARAMETERS"]; exists {
		filtered, err := filterIdentityParameters(raw)
		if err != nil {
			return err
		}
		set("GIT_CONFIG_PARAMETERS", filtered)
	}
	return nil
}

func identityConfigKey(key string) bool {
	key = strings.ToLower(key)
	return strings.HasPrefix(key, "author.") || strings.HasPrefix(key, "committer.") || key == "user.name" || key == "user.email"
}

// Follow Git's parse_config_env_list / sq_dequote_step grammar, accepting both
// 'key=value' and 'key'='value' (or 'key'= for an implicit boolean). Retained
// tokens stay byte-for-byte intact. This is Git quoting, never shell evaluation.
func filterIdentityParameters(raw string) (string, error) {
	var retained []string
	for position := 0; position < len(raw); {
		start := position
		key, next, ok := configParameterQuote(raw, position)
		if !ok {
			return "", fmt.Errorf("cannot isolate commit identity: malformed inherited GIT_CONFIG_PARAMETERS")
		}
		position = next
		if position < len(raw) && raw[position] == '=' {
			position++
			if position < len(raw) && !configParameterSpace(raw[position]) {
				_, position, ok = configParameterQuote(raw, position)
			}
		} else {
			key, _, _ = strings.Cut(key, "=")
		}
		if !ok || (position < len(raw) && !configParameterSpace(raw[position])) {
			return "", fmt.Errorf("cannot isolate commit identity: malformed inherited GIT_CONFIG_PARAMETERS")
		}
		if !identityConfigKey(key) {
			retained = append(retained, raw[start:position])
		}
		for position < len(raw) && configParameterSpace(raw[position]) {
			position++
		}
	}
	return strings.Join(retained, " "), nil
}

func configParameterSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func configParameterQuote(raw string, position int) (string, int, bool) {
	if position >= len(raw) || raw[position] != '\'' {
		return "", position, false
	}
	var value strings.Builder
	for position++; position < len(raw); position++ {
		if raw[position] != '\'' {
			value.WriteByte(raw[position])
			continue
		}
		position++
		if position+2 < len(raw) && raw[position] == '\\' && (raw[position+1] == '\'' || raw[position+1] == '!') && raw[position+2] == '\'' {
			value.WriteByte(raw[position+1])
			position += 2
			continue
		}
		return value.String(), position, true
	}
	return "", position, false
}
