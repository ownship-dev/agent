package compose

import (
	"fmt"
	"strings"
)

// interpolate expands compose variables ($VAR, ${VAR:-d}, ${VAR:?e}, $$...) here, so the
// sanitizer judges the exact values that will run.
func interpolate(s string, env map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i++
		case next == '{':
			end, err := closingBrace(s, i+2)
			if err != nil {
				return "", err
			}
			val, err := expandBraced(s[i+2:end], env)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i = end
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			b.WriteString(env[s[i+1:j]])
			i = j - 1
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// closingBrace allows nested ${...} in defaults.
func closingBrace(s string, start int) (int, error) {
	depth := 1
	for j := start; j < len(s); j++ {
		switch {
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case s[j] == '}':
			depth--
			if depth == 0 {
				return j, nil
			}
		}
	}
	return 0, fmt.Errorf("unclosed ${ in %q", s)
}

func expandBraced(expr string, env map[string]string) (string, error) {
	j := 0
	for j < len(expr) && isNameChar(expr[j]) {
		j++
	}
	name, rest := expr[:j], expr[j:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("invalid variable ${%s}", expr)
	}
	val, set := env[name]
	if rest == "" {
		return val, nil
	}
	for _, op := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if !strings.HasPrefix(rest, op) {
			continue
		}
		arg, err := interpolate(rest[len(op):], env)
		if err != nil {
			return "", err
		}
		// The ":" forms treat an empty value as unset.
		missing := !set || (op[0] == ':' && val == "")
		switch op[len(op)-1] {
		case '-':
			if missing {
				return arg, nil
			}
			return val, nil
		case '?':
			if missing {
				return "", fmt.Errorf("required variable %s is missing: %s", name, arg)
			}
			return val, nil
		case '+':
			if missing {
				return "", nil
			}
			return arg, nil
		}
	}
	return "", fmt.Errorf("invalid variable ${%s}", expr)
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }
