package gitlab

import (
	"strings"
	"unicode"
)

// NeutralizeQuickActions rewrites every line that GitLab would interpret as a
// quick action. GitLab executes any note line that starts with "/" (after
// optional whitespace) under the posting account, so untrusted text such as
// "/close" or "/approve" must never reach the API unchanged. Outside fenced
// code blocks the leading slash becomes "&#47;", which renders as "/" without
// being executed. Fenced blocks (``` and ~~~, up to three spaces of indent)
// are left alone, as is everything that does not start a line with a slash:
// inline code, blockquotes and list items are not parsed as quick actions.
// A fence that is never closed is not trusted as code, so its content is
// neutralized too. The function is idempotent.
func NeutralizeQuickActions(body string) string {
	if !strings.Contains(body, "/") {
		return body
	}
	lines := splitKeepEOL(body)
	protected := fencedLines(lines)
	var b strings.Builder
	b.Grow(len(body) + 16)
	for i, line := range lines {
		if !protected[i] {
			line = neutralizeLine(line)
		}
		b.WriteString(line)
	}
	return b.String()
}

// splitKeepEOL splits on \n, \r\n and lone \r, keeping each terminator.
func splitKeepEOL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, s[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func neutralizeLine(line string) string {
	for i, r := range line {
		if r == '/' {
			return line[:i] + "&#47;" + line[i+1:]
		}
		if !unicode.IsSpace(r) {
			return line
		}
	}
	return line
}

// fencedLines marks the lines that belong to a closed fenced code block,
// fence lines included.
func fencedLines(lines []string) []bool {
	protected := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		ch, n, ok := fenceOpen(lines[i])
		if !ok {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if fenceClose(lines[j], ch, n) {
				for k := i; k <= j; k++ {
					protected[k] = true
				}
				i = j
				break
			}
		}
	}
	return protected
}

func trimEOL(line string) string { return strings.TrimRight(line, "\r\n") }

func fenceOpen(line string) (byte, int, bool) {
	line = trimEOL(line)
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 || (ch == '`' && strings.ContainsRune(trimmed[n:], '`')) {
		return 0, 0, false
	}
	return ch, n, true
}

func fenceClose(line string, ch byte, min int) bool {
	line = trimEOL(line)
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return n >= min && strings.Trim(trimmed[n:], " \t") == ""
}
