package atmodem

import "strings"

// field returns the i-th comma-separated value after the colon of an answer line.
func field(line string, i int) string {
	if k := strings.Index(line, ":"); k >= 0 {
		line = line[k+1:]
	}
	fields := splitFields(strings.TrimSpace(line))
	if i >= len(fields) {
		return ""
	}
	return fields[i]
}

// splitFields splits on commas that are not inside quotes.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func unquote(s string) string { return strings.Trim(s, `"`) }

func validNumber(n string) bool {
	if len(n) < 4 || len(n) > 16 {
		return false
	}
	for i, r := range n {
		if !(r >= '0' && r <= '9') && !(i == 0 && r == '+') {
			return false
		}
	}
	return true
}

// clean keeps a text safe for text mode: no control characters (Ctrl-Z would end the
// message, ESC would cancel it), plain ASCII.
func clean(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, text)
}
