// Package search runs filtered text searches across a project root by shelling
// out to ripgrep. Results are returned grouped by file so the UI can render a
// tree of files → match lines.
package search

import (
	"bufio"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// Options control a single search.
type Options struct {
	Query         string   // pattern (literal unless Regex is true)
	Root          string   // project directory to search under
	Regex         bool     // treat Query as a regex
	CaseSensitive bool     // off → smart-case
	WholeWord     bool     // match whole word only
	Includes      []string // ripgrep --glob includes (e.g. "*.go")
	Excludes      []string // ripgrep --glob !excludes (e.g. "vendor/**")
	MaxResults    int      // cap total match lines (0 → 2000)
}

// Match is a single hit inside a file.
type Match struct {
	Line   int `json:"line"`
	Column int `json:"column"` // 1-based start of the matched span
	// MatchLen is the byte length of the matched span (submatch end - start),
	// so the UI can highlight exactly the matched text even for regex queries.
	// 0 when ripgrep reported no submatch.
	MatchLen int    `json:"matchLen"`
	Preview  string `json:"preview"`
}

// FileMatches groups hits by file.
type FileMatches struct {
	Path    string  `json:"path"`
	Matches []Match `json:"matches"`
	// Capped is true when this file hit ripgrep's per-file --max-count limit,
	// so more matches exist on disk than are listed here. The UI surfaces this
	// so the count isn't read as exhaustive.
	Capped bool `json:"capped"`
}

// Result is what the IPC handler returns.
type Result struct {
	Files     []FileMatches `json:"files"`
	Truncated bool          `json:"truncated"`
	Total     int           `json:"total"`
}

// Run executes ripgrep and parses its JSON event stream.
func Run(opts Options) (*Result, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return &Result{Files: []FileMatches{}}, nil
	}
	if opts.Root == "" {
		return nil, errors.New("search: root required")
	}
	limit := opts.MaxResults
	if limit <= 0 {
		limit = 2000
	}

	// Per-file match cap. Kept as a named constant so the parser can flag files
	// that hit it (FileMatches.Capped) rather than silently truncating.
	const perFileMax = 200

	args := []string{"--json", "--no-messages", "--max-count=" + strconv.Itoa(perFileMax)}
	if !opts.Regex {
		args = append(args, "--fixed-strings")
	}
	if opts.WholeWord {
		args = append(args, "--word-regexp")
	}
	if opts.CaseSensitive {
		args = append(args, "--case-sensitive")
	} else {
		args = append(args, "--smart-case")
	}
	for _, g := range opts.Includes {
		if g = strings.TrimSpace(g); g != "" {
			args = append(args, "--glob", g)
		}
	}
	for _, g := range opts.Excludes {
		if g = strings.TrimSpace(g); g != "" {
			args = append(args, "--glob", "!"+g)
		}
	}
	args = append(args, "--", opts.Query, opts.Root)

	cmd := exec.Command("rg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	byPath := map[string]*FileMatches{}
	order := []string{}
	total := 0
	truncated := false

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if total >= limit {
			truncated = true
			break
		}
		var evt struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil || evt.Type != "match" {
			continue
		}
		var m struct {
			Path struct {
				Text string `json:"text"`
			} `json:"path"`
			Lines struct {
				Text string `json:"text"`
			} `json:"lines"`
			LineNumber int `json:"line_number"`
			Submatches []struct {
				Start int `json:"start"`
				End   int `json:"end"`
			} `json:"submatches"`
		}
		if err := json.Unmarshal(evt.Data, &m); err != nil {
			continue
		}
		col := 1
		matchLen := 0
		if len(m.Submatches) > 0 {
			col = m.Submatches[0].Start + 1
			if n := m.Submatches[0].End - m.Submatches[0].Start; n > 0 {
				matchLen = n
			}
		}
		fm, ok := byPath[m.Path.Text]
		if !ok {
			fm = &FileMatches{Path: m.Path.Text}
			byPath[m.Path.Text] = fm
			order = append(order, m.Path.Text)
		}
		fm.Matches = append(fm.Matches, Match{
			Line:     m.LineNumber,
			Column:   col,
			MatchLen: matchLen,
			Preview:  strings.TrimRight(m.Lines.Text, "\n"),
		})
		total++
	}
	if truncated && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	// rg exits non-zero on "no matches" (1) and on errors (2). Ignore both —
	// we either got matches or we didn't.
	_ = cmd.Wait()

	files := make([]FileMatches, 0, len(order))
	for _, p := range order {
		fm := byPath[p]
		// ripgrep stops at perFileMax matches per file; a file at that exact
		// count almost certainly has more on disk. Flag it for the UI.
		if len(fm.Matches) >= perFileMax {
			fm.Capped = true
		}
		files = append(files, *fm)
	}
	return &Result{Files: files, Truncated: truncated, Total: total}, nil
}
