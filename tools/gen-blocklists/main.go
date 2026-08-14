// Command gen-blocklists builds the embedded blocklistproject category assets
// shipped in lists/blocklists/<cat>.txt.gz plus a manifest.json. It is run
// manually (NOT part of `go build`):
//
//	go run ./tools/gen-blocklists                 # fetch all categories
//	go run ./tools/gen-blocklists -only ads,tracking,scam
//	go run ./tools/gen-blocklists -skip malware,porn
//
// For each category it fetches the domain-only alt-version file
// (https://raw.githubusercontent.com/blocklistproject/Lists/main/alt-version/<cat>-nl.txt),
// strips comment (#) and blank lines, normalizes each entry (lowercase, trim,
// strip trailing dot), and gzips one file per category. It records the repo's
// current main commit SHA into lists/blocklists/manifest.json so the seeding /
// updater subsystem can version-gate against it. Keeping one gz per category
// lets a build select a subset via build tags later; the runtime handles any
// subset present.
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// allCategories is the full blocklistproject alt-version domain-only set.
var allCategories = []string{
	"ads", "tracking", "fraud", "phishing", "scam", "malware", "abuse",
	"gambling", "porn", "redirect", "basic", "youtube", "facebook", "drugs",
	"tiktok", "ransomware", "torrent", "piracy", "twitter", "crypto", "adobe",
	"whatsapp", "vaping", "smart-tv", "fortnite",
}

const (
	rawBase   = "https://raw.githubusercontent.com/blocklistproject/Lists/main/alt-version/%s-nl.txt"
	commitAPI = "https://api.github.com/repos/blocklistproject/Lists/commits/main"
)

type manifest struct {
	Commit     string        `json:"commit"`
	FetchedAt  string        `json:"fetchedAt"`
	Categories []catManifest `json:"categories"`
}

type catManifest struct {
	Name    string `json:"name"`
	Domains int    `json:"domains"`
	Bytes   int    `json:"bytes"` // gzipped size on disk
}

func main() {
	outDir := flag.String("out", "lists/blocklists", "output directory for <cat>.txt.gz + manifest.json")
	only := flag.String("only", "", "comma-separated subset of categories to fetch (default all)")
	skip := flag.String("skip", "", "comma-separated categories to skip")
	flag.Parse()

	cats := selectCategories(*only, *skip)
	if len(cats) == 0 {
		log.Fatal("no categories selected")
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil { //nolint:gosec,mnd
		log.Fatalf("mkdir: %v", err)
	}

	commit, err := fetchCommit()
	if err != nil {
		log.Fatalf("fetch commit sha: %v", err)
	}
	log.Printf("blocklistproject main commit: %s", commit)

	man := manifest{Commit: commit, FetchedAt: time.Now().UTC().Format(time.RFC3339)}

	for _, c := range cats {
		n, nbytes, err := fetchCategory(c, *outDir)
		if err != nil {
			log.Fatalf("category %s: %v", c, err)
		}
		log.Printf("%-12s %7d domains -> %d bytes gz", c, n, nbytes)
		man.Categories = append(man.Categories, catManifest{Name: c, Domains: n, Bytes: nbytes})
	}

	if err := writeManifest(filepath.Join(*outDir, "manifest.json"), man); err != nil {
		log.Fatalf("write manifest: %v", err)
	}

	total := 0
	for _, c := range man.Categories {
		total += c.Bytes
	}
	log.Printf("wrote %d categories, %d bytes gz total, manifest commit %s", len(man.Categories), total, commit)
}

func selectCategories(only, skip string) []string {
	base := allCategories
	if only != "" {
		base = splitCSV(only)
	}

	skipSet := map[string]bool{}
	for _, s := range splitCSV(skip) {
		skipSet[s] = true
	}

	out := make([]string, 0, len(base))
	for _, c := range base {
		if !skipSet[c] {
			out = append(out, c)
		}
	}

	return out
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// fetchCategory downloads one category, strips comments/blanks, normalizes,
// gzips to <outDir>/<cat>.txt.gz, and returns (domainCount, gzBytesOnDisk).
func fetchCategory(cat, outDir string) (int, int, error) {
	//nolint:noctx,gosec // one-shot dev tool
	resp, err := http.Get(fmt.Sprintf(rawBase, cat))
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("unexpected status %s", resp.Status)
	}

	path := filepath.Join(outDir, cat+".txt.gz")
	f, err := os.Create(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	gw.Comment = "blocklistproject/" + cat
	bw := bufio.NewWriter(gw)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) //nolint:mnd
	count := 0

	for scanner.Scan() {
		d := normalize(scanner.Text())
		if d == "" {
			continue
		}

		if _, err := bw.WriteString(d + "\n"); err != nil {
			return count, 0, err
		}
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, 0, err
	}

	if err := bw.Flush(); err != nil {
		return count, 0, err
	}
	if err := gw.Close(); err != nil {
		return count, 0, err
	}
	if err := f.Close(); err != nil {
		return count, 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return count, 0, err
	}

	return count, int(info.Size()), nil
}

// normalize drops comment (#) and blank lines and lowercases/trims the domain,
// stripping a trailing dot. Returns "" for anything that isn't a plausible
// domain (no dot, contains whitespace, leading dot).
func normalize(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}

	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")

	if s == "" || !strings.Contains(s, ".") || strings.ContainsAny(s, " \t") || strings.HasPrefix(s, ".") {
		return ""
	}

	return s
}

func fetchCommit() (string, error) {
	//nolint:noctx // one-shot dev tool
	resp, err := http.Get(commitAPI)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}

	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.SHA == "" {
		return "", errors.New("empty sha in commit response")
	}

	return body.SHA, nil
}

func writeManifest(path string, m manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, append(b, '\n'), 0o644) //nolint:gosec,mnd
}
