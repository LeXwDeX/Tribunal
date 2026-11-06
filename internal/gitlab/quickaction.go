package gitlab

import (
	"strings"
)

// NeutralizeQuickActions rewrites every line that GitLab would interpret as a
// quick action. GitLab executes a line whose first byte is "/" in a top-level
// paragraph under the posting account, so untrusted text such as "/close" or
// "/approve" must never reach the API unchanged. Such a line gets "&#47;" in
// place of the slash, which renders as "/" without being executed.
//
// Only a closed fenced code block that starts at column 0 and sits at the top
// level is left alone. A fence is not trusted when a list item, blockquote or
// HTML line (or an unclosed HTML comment or raw HTML block) precedes it in the
// same paragraph, because GitLab then parses the "code" as ordinary text. An
// unclosed fence is not trusted either. Lines that do not start with "/"
// (indented, inline code, blockquote, list item) are never changed. The
// function is idempotent.
func NeutralizeQuickActions(body string) string {
	if !strings.Contains(body, "/") {
		return body
	}
	lines := splitKeepEOL(body)
	protected := fencedLines(lines)
	var b strings.Builder
	b.Grow(len(body) + 16)
	for i, line := range lines {
		if !protected[i] && strings.HasPrefix(line, "/") {
			line = "&#47;" + line[1:]
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

// fencedLines marks the lines of every trusted fenced code block, fence lines
// included.
func fencedLines(lines []string) []bool {
	protected := make([]bool, len(lines))
	suspect := false // a list, quote or HTML line since the last blank line
	rawEnd := ""     // closing token of an open HTML comment or raw block
	for i := 0; i < len(lines); i++ {
		line := trimEOL(lines[i])
		if rawEnd != "" {
			if strings.Contains(strings.ToLower(line), rawEnd) {
				rawEnd = ""
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			suspect = false
			continue
		}
		if !suspect && fenceAtColumnZero(line) {
			ch, n, _ := fenceOpen(line)
			closed := -1
			for j := i + 1; j < len(lines); j++ {
				if fenceClose(lines[j], ch, n) {
					closed = j
					break
				}
			}
			if closed > 0 {
				for k := i; k <= closed; k++ {
					protected[k] = true
				}
				i = closed
				continue
			}
		}
		if startsSuspect(line) {
			suspect = true
		}
		rawEnd = unclosedRawHTML(line)
	}
	return protected
}

func fenceAtColumnZero(line string) bool {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return false
	}
	_, _, ok := fenceOpen(line)
	return ok
}

// startsSuspect reports whether a line can make a following fence part of a
// list item, blockquote or HTML block instead of a top-level code block.
func startsSuspect(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if t == "" {
		return false
	}
	switch t[0] {
	case '<', '>':
		return true
	case '-', '*', '+':
		return len(t) == 1 || t[1] == ' ' || t[1] == '\t'
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i > 0 && i < len(t) && (t[i] == '.' || t[i] == ')') && (i+1 == len(t) || t[i+1] == ' ' || t[i+1] == '\t')
}

// unclosedRawHTML returns the token that ends an HTML comment or raw HTML
// block opened on this line and still open at its end, or "".
func unclosedRawHTML(line string) string {
	lower := strings.ToLower(line)
	for _, open := range [][2]string{{"<!--", "-->"}, {"<pre", "</pre>"}, {"<script", "</script>"}, {"<style", "</style>"}, {"<textarea", "</textarea>"}} {
		if i := strings.LastIndex(lower, open[0]); i >= 0 && !strings.Contains(lower[i:], open[1]) {
			return open[1]
		}
	}
	return ""
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
