package runner

import (
	"bytes"
	"regexp"
)

// resultLine is the line a tool ends on: RESULT: <word>( <detail>)?
var resultLine = regexp.MustCompile(`^RESULT: ([a-z][a-z-]*)(?: (.*))?$`)

// maxLine is the longest line checked for a RESULT. Anything longer is agent
// output, not a result, and is not buffered.
const maxLine = 4096

// resultScanner remembers the last RESULT line written through it, without
// keeping the rest of the output: an agent's log can run to megabytes.
type resultScanner struct {
	partial  []byte
	overflow bool
	word     string
	detail   string
	found    bool
}

func (s *resultScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			s.buffer(p)
			break
		}
		s.buffer(p[:end])
		s.line()
		p = p[end+1:]
	}
	return n, nil
}

func (s *resultScanner) buffer(p []byte) {
	if s.overflow || len(s.partial)+len(p) > maxLine {
		s.overflow, s.partial = true, s.partial[:0]
		return
	}
	s.partial = append(s.partial, p...)
}

func (s *resultScanner) line() {
	if !s.overflow {
		if match := resultLine.FindSubmatch(bytes.TrimRight(s.partial, "\r")); match != nil {
			s.word, s.detail, s.found = string(match[1]), string(match[2]), true
		}
	}
	s.partial, s.overflow = s.partial[:0], false
}

// result returns the last RESULT line, counting an unterminated last line.
func (s *resultScanner) result() (word, detail string, ok bool) {
	if len(s.partial) > 0 || s.overflow {
		s.line()
	}
	return s.word, s.detail, s.found
}
