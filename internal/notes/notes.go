// Package notes turns directories of markdown files into attention items. A
// voice-note capture lands as a file; this source makes each one visible until
// somebody moves or deletes it. It only reads.
package notes

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// SourceName is the item source this adapter owns.
const SourceName = "notes"

// Label is the word every note carries. Mourningstar reads it, so it is part
// of the contract rather than a display choice.
const Label = "idea"

// MaxSize is the largest note read. A bigger file is not a captured idea.
const MaxSize = 1 << 20

// Dir is one watched directory.
type Dir struct {
	Path  string
	Label string
}

// Problem is something wrong with one directory or file that did not stop the
// scan. Key is stable between polls so the caller can log each problem once.
type Problem struct {
	Key     string
	Message string
}

// Scan lists the top level of each directory and returns one item per
// markdown file. A directory that cannot be read yields a Problem and no
// items. A file that cannot be read or is too large yields a Problem and is
// skipped. Items come back sorted by id.
func Scan(dirs []Dir) ([]attention.Item, []Problem) {
	var (
		items    []attention.Item
		problems []Problem
	)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.Path)
		if err != nil {
			problems = append(problems, Problem{
				Key:     "dir:" + dir.Label,
				Message: fmt.Sprintf("notes directory %q is unreadable: %v", dir.Label, err),
			})
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
				continue
			}
			path := filepath.Join(dir.Path, name)
			info, err := os.Stat(path) // follows a symlink to a file
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if info.Size() > MaxSize {
				problems = append(problems, Problem{
					Key:     "size:" + path,
					Message: fmt.Sprintf("note %s/%s is larger than %d bytes, skipped", dir.Label, name, MaxSize),
				})
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				problems = append(problems, Problem{
					Key:     "read:" + path,
					Message: fmt.Sprintf("note %s/%s is unreadable: %v", dir.Label, name, err),
				})
				continue
			}
			items = append(items, newItem(dir, name, raw, info.ModTime()))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, problems
}

func newItem(dir Dir, name string, raw []byte, mtime time.Time) attention.Item {
	sum := sha256.Sum256(raw)
	front, body := splitFrontmatter(raw)

	title := heading(body)
	if title == "" {
		title = strings.TrimSuffix(name, ".md")
	}

	meta := map[string]string{
		"path":   filepath.Join(dir.Path, name),
		"dir":    dir.Path,
		"sha256": hex.EncodeToString(sum[:]),
	}
	if captured := parseDate(front["date"]); captured != "" {
		meta["captured"] = captured
	}
	if app := front["app"]; app != "" {
		meta["app"] = app
	}

	return attention.Item{
		ID:        attention.Key(SourceName, dir.Label+"/"+name),
		Source:    SourceName,
		Title:     title,
		State:     attention.StateNeedsAttention,
		Severity:  attention.SeverityInfo,
		Label:     Label,
		Tone:      attention.ToneAttention,
		Priority:  attention.DefaultPriority(attention.StateNeedsAttention),
		Context:   meta,
		UpdatedAt: mtime,
	}
}

// splitFrontmatter reads a leading "---" block as plain "key: value" lines. It
// is not YAML: nested values, lists and multi-line strings are ignored. A
// block that never closes is not frontmatter, so the body is the whole file.
func splitFrontmatter(raw []byte) (map[string]string, []byte) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	first, rest, ok := bytes.Cut(raw, []byte("\n"))
	if !ok || strings.TrimSpace(string(first)) != "---" {
		return nil, raw
	}

	front := map[string]string{}
	for len(rest) > 0 {
		line, after, _ := bytes.Cut(rest, []byte("\n"))
		text := strings.TrimRight(string(line), "\r")
		if strings.TrimSpace(text) == "---" {
			return front, after
		}
		rest = after
		key, value, found := strings.Cut(text, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "date" && key != "app" {
			continue
		}
		front[key] = unquote(strings.TrimSpace(value))
	}
	return nil, raw
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// parseDate returns an RFC 3339 string for a frontmatter date, or "" when the
// value is empty or not a date. A date with no zone is read as UTC.
func parseDate(s string) string {
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format(time.RFC3339)
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return ""
}

// heading returns the first "# " heading outside a code fence.
func heading(body []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxSize)
	fenced := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if title, ok := strings.CutPrefix(line, "# "); ok {
			if title = strings.TrimSpace(title); title != "" {
				return title
			}
		}
	}
	return ""
}
