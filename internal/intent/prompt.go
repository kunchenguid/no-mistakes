package intent

import "strings"

func CleanForPrompt(text string) string {
	text = strings.NewReplacer("<<<<<<<", " ", "=======", " ", ">>>>>>>", " ").Replace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.Join(strings.Fields(lines[i]), " ")
	}
	return RedactSecrets(StripAdversarial(strings.TrimSpace(strings.Join(lines, "\n"))))
}
