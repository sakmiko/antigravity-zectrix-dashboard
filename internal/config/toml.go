package config

import (
	"fmt"
	"strconv"
	"strings"
)

// parseTOML parses a clean subset of TOML (tables, strings, integers, booleans,
// string arrays, and comments) into a flat dotted-key map without external dependencies.
func parseTOML(src string) (map[string]any, error) {
	out := make(map[string]any)
	var section string

	lines := splitLinesClean(src)
	i := 0
	for i < len(lines) {
		rawLine := lines[i]
		i++
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: malformed table header %q", i, line)
			}
			sec := strings.TrimSpace(line[1 : len(line)-1])
			sec = strings.Trim(sec, `"'`)
			if sec == "" {
				return nil, fmt.Errorf("line %d: empty table header", i)
			}
			section = sec
			continue
		}

		eqIdx := findUnquotedByte(line, '=')
		if eqIdx <= 0 {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", i, line)
		}
		key := strings.TrimSpace(line[:eqIdx])
		key = strings.Trim(key, `"'`)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i)
		}
		valPart := strings.TrimSpace(line[eqIdx+1:])

		// Handle multi-line arrays `[` ... `]`
		if strings.HasPrefix(valPart, "[") && !arrayClosed(valPart) {
			var b strings.Builder
			b.WriteString(valPart)
			for i < len(lines) {
				next := strings.TrimSpace(stripTOMLComment(lines[i]))
				i++
				if next == "" {
					continue
				}
				b.WriteByte(' ')
				b.WriteString(next)
				if arrayClosed(b.String()) {
					break
				}
			}
			valPart = b.String()
		}

		val, err := parseTOMLValue(valPart)
		if err != nil {
			return nil, fmt.Errorf("line %d key %q: %w", i, key, err)
		}
		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}
		out[fullKey] = val
	}
	return out, nil
}

func splitLinesClean(s string) []string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

func stripTOMLComment(line string) string {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return line[:i]
		}
	}
	return line
}

func findUnquotedByte(s string, target byte) int {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		default:
			if c == target {
				return i
			}
		}
	}
	return -1
}

func arrayClosed(s string) bool {
	inDouble := false
	inSingle := false
	escaped := false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '[':
			depth++
		case ']':
			depth--
			if depth <= 0 {
				return true
			}
		}
	}
	return depth <= 0
}

func parseTOMLValue(raw string) (any, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty value")
	}
	if strings.HasPrefix(s, `"`) {
		if !strings.HasSuffix(s, `"`) || len(s) < 2 {
			return nil, fmt.Errorf("unterminated string %q", s)
		}
		unq, err := strconv.Unquote(s)
		if err != nil {
			return nil, err
		}
		return unq, nil
	}
	if strings.HasPrefix(s, `'`) {
		if !strings.HasSuffix(s, `'`) || len(s) < 2 {
			return nil, fmt.Errorf("unterminated literal string %q", s)
		}
		return s[1 : len(s)-1], nil
	}
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("unterminated array %q", s)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []string{}, nil
		}
		var items []string
		cur := inner
		for cur != "" {
			comma := findUnquotedByte(cur, ',')
			var elem string
			if comma < 0 {
				elem = strings.TrimSpace(cur)
				cur = ""
			} else {
				elem = strings.TrimSpace(cur[:comma])
				cur = strings.TrimSpace(cur[comma+1:])
			}
			if elem == "" {
				continue
			}
			v, err := parseTOMLValue(elem)
			if err != nil {
				return nil, err
			}
			items = append(items, fmt.Sprint(v))
		}
		return items, nil
	}
	if s == "true" {
		return true, nil
	}
	if s == "false" {
		return false, nil
	}
	cleaned := strings.ReplaceAll(s, "_", "")
	if n, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
		return n, nil
	}
	return nil, fmt.Errorf("unsupported TOML value %q", s)
}
