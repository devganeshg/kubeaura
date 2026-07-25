package rag

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Chunk is a searchable documentation segment.
type Chunk struct {
	Path    string
	Heading string
	Content string
}

// Retriever holds pre-indexed markdown chunks for simple in-process RAG.
type Retriever struct {
	chunks []Chunk
}

// NewFromURL indexes MkDocs material search index from a docs site, e.g.
// https://docs.example.com/docs/.
func NewFromURL(baseURL string) (*Retriever, error) {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return nil, fmt.Errorf("empty docs base url")
	}
	base = strings.TrimRight(base, "/") + "/"
	idxURL := base + "search/search_index.json"

	req, err := http.NewRequest(http.MethodGet, idxURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("docs index fetch failed: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Docs []struct {
			Location string `json:"location"`
			Title    string `json:"title"`
			Text     string `json:"text"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, fmt.Errorf("invalid docs index json: %w", err)
	}

	chunks := make([]Chunk, 0, len(payload.Docs))
	for _, d := range payload.Docs {
		path := normalizeLocation(base, d.Location)
		txt := cleanDocText(d.Text)
		if strings.TrimSpace(txt) == "" {
			continue
		}
		for _, part := range splitBySize(txt, 1400) {
			chunks = append(chunks, Chunk{
				Path:    path,
				Heading: strings.TrimSpace(d.Title),
				Content: part,
			})
		}
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no indexable docs from %s", idxURL)
	}
	return &Retriever{chunks: chunks}, nil
}

// NewFromDir recursively indexes markdown files under docsDir.
func NewFromDir(docsDir string) (*Retriever, error) {
	var files []string
	err := filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	chunks := make([]Chunk, 0, len(files)*2)
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rel := filepath.ToSlash(p)
		chunks = append(chunks, splitMarkdown(rel, string(b))...)
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("no markdown documents indexed from %s", docsDir)
	}
	return &Retriever{chunks: chunks}, nil
}

// Search returns top-k chunks relevant to query.
func (r *Retriever) Search(query string, k int) []Chunk {
	if r == nil || len(r.chunks) == 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	if k <= 0 {
		k = 5
	}

	q := tokenFreq(query)
	type scored struct {
		idx   int
		score int
	}
	scores := make([]scored, 0, len(r.chunks))
	for i, c := range r.chunks {
		text := c.Heading + "\n" + c.Content
		freq := tokenFreq(text)
		s := 0
		for tok, qv := range q {
			if fv, ok := freq[tok]; ok {
				s += qv * fv
			}
		}
		if s > 0 {
			scores = append(scores, scored{idx: i, score: s})
		}
	}

	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].idx < scores[j].idx
		}
		return scores[i].score > scores[j].score
	})

	if len(scores) > k {
		scores = scores[:k]
	}
	out := make([]Chunk, 0, len(scores))
	for _, s := range scores {
		out = append(out, r.chunks[s.idx])
	}
	return out
}

// ContextForPrompt formats top-k hits into prompt-ready context with citations.
func (r *Retriever) ContextForPrompt(query string, k int) string {
	hits := r.Search(query, k)
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("DOCUMENT CONTEXT (team docs):\n")
	for i, h := range hits {
		b.WriteString(fmt.Sprintf("[%d] source=%s", i+1, h.Path))
		if h.Heading != "" {
			b.WriteString(" | heading=")
			b.WriteString(h.Heading)
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(h.Content))
		b.WriteString("\n\n")
	}
	return b.String()
}

func splitMarkdown(path, content string) []Chunk {
	lines := strings.Split(content, "\n")
	chunks := make([]Chunk, 0)
	heading := ""
	var cur strings.Builder
	flush := func() {
		text := strings.TrimSpace(cur.String())
		if text == "" {
			cur.Reset()
			return
		}
		for _, part := range splitBySize(text, 1400) {
			chunks = append(chunks, Chunk{Path: path, Heading: heading, Content: part})
		}
		cur.Reset()
	}

	for _, l := range lines {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "#") {
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(trim, "#"))
			continue
		}
		cur.WriteString(l)
		cur.WriteString("\n")
	}
	flush()
	return chunks
}

func splitBySize(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	parts := make([]string, 0, len(s)/max+1)
	for len(s) > max {
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/3 {
			cut = strings.LastIndex(s[:max], " ")
		}
		if cut <= 0 {
			cut = max
		}
		parts = append(parts, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		parts = append(parts, s)
	}
	return parts
}

func tokenFreq(s string) map[string]int {
	toks := tokenize(s)
	m := make(map[string]int, len(toks))
	for _, t := range toks {
		if len(t) < 3 {
			continue
		}
		m[t]++
	}
	return m
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func cleanDocText(s string) string {
	out := html.UnescapeString(s)
	out = htmlTagRe.ReplaceAllString(out, " ")
	out = strings.ReplaceAll(out, "\u00a0", " ")
	out = strings.Join(strings.Fields(out), " ")
	return strings.TrimSpace(out)
}

func normalizeLocation(baseURL, loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return baseURL
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	if strings.HasPrefix(loc, "#") {
		return baseURL + loc
	}
	return baseURL + strings.TrimLeft(loc, "/")
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}
