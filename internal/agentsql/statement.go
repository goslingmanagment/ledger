package agentsql

import (
	"errors"
	"fmt"
	"strings"
)

// MaxSQL is the longest statement accepted, in bytes.
const MaxSQL = 64 << 10

// Statement returns the one statement of query without the semicolons and blanks around it.
// It reads quotes and comments the way SQLite's tokenizer does, so a ';' inside '…', "…", `…`,
// […], -- … or /* … */ splits nothing. An empty query, a NUL byte, more than MaxSQL bytes or a
// second statement is an error: given a script, the driver runs every statement of it.
func Statement(query string) (string, error) {
	if len(query) > MaxSQL {
		return "", fmt.Errorf("запрос длиннее %d КБ", MaxSQL>>10)
	}
	if strings.IndexByte(query, 0) >= 0 {
		return "", errors.New("в запросе нулевой байт")
	}
	var stmts []string
	start, code := 0, false
	for i := 0; i < len(query); {
		switch c := query[i]; {
		case c == ';':
			if code {
				stmts = append(stmts, query[start:i])
			}
			start, code = i+1, false
			i++
		case strings.HasPrefix(query[i:], "--"):
			i = skipPast(query, i+2, "\n")
		case strings.HasPrefix(query[i:], "/*"):
			i = skipPast(query, i+2, "*/")
		case c == '\'' || c == '"' || c == '`':
			i, code = skipQuoted(query, i), true
		case c == '[':
			i, code = skipPast(query, i+1, "]"), true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		default:
			i, code = i+1, true
		}
	}
	if code {
		stmts = append(stmts, query[start:])
	}
	switch len(stmts) {
	case 0:
		return "", errors.New("пустой запрос")
	case 1:
		return strings.TrimSpace(stmts[0]), nil
	}
	return "", errors.New("в запросе больше одной команды: можно одну")
}

// verb is the first keyword of stmt in capitals (CREATE, DROP, ANALYZE…), "" when it does
// not start with a word.
func verb(stmt string) string {
	for i := 0; i < len(stmt); {
		switch c := stmt[i]; {
		case strings.HasPrefix(stmt[i:], "--"):
			i = skipPast(stmt, i+2, "\n")
		case strings.HasPrefix(stmt[i:], "/*"):
			i = skipPast(stmt, i+2, "*/")
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		default:
			j := i
			for j < len(stmt) && (stmt[j]|0x20 >= 'a' && stmt[j]|0x20 <= 'z') {
				j++
			}
			return strings.ToUpper(stmt[i:j])
		}
	}
	return ""
}

// skipPast returns the index just past the first end at or after i, or len(s).
func skipPast(s string, i int, end string) int {
	if j := strings.Index(s[i:], end); j >= 0 {
		return i + j + len(end)
	}
	return len(s)
}

// skipQuoted returns the index just past the quote that closes the one at i; a doubled quote
// inside is an escaped one. Unclosed, it runs to the end, as in SQLite.
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}
