// Command gen-decoy-list builds the embedded decoy domain list shipped in
// decoy/tranco-1m.txt.gz. It is run manually (NOT part of `go build`):
//
//	go run ./tools/gen-decoy-list -out decoy/tranco-1m.txt.gz
//
// By default it fetches a pinned Tranco snapshot (https://tranco-list.eu), keeps
// all ~1,000,000 entries (no eTLD+1 dedup — the engine wants the full list),
// normalizes each (lowercase, strip trailing dot, drop obviously-invalid lines)
// and gzips the result. The Tranco list ID and fetch date are written to a
// sidecar decoy/tranco-1m.txt.gz.source so the snapshot is reproducible.
//
// To refresh the real list: pick a list ID from https://tranco-list.eu/ (the
// permanent ID under "Download"), then:
//
//	go run ./tools/gen-decoy-list -id <LIST_ID> -out decoy/tranco-1m.txt.gz
//
// Tranco publishes the ranked list as a zip containing top-1m.csv with lines
// "rank,domain"; this tool reads that CSV directly from the zip.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	out := flag.String("out", "decoy/tranco-1m.txt.gz", "output gzip path")
	id := flag.String("id", "", "Tranco permanent list ID (empty = use -url)")
	url := flag.String("url", "", "explicit download URL of a Tranco top-1m zip (overrides -id)")
	flag.Parse()

	src := *url
	if src == "" && *id != "" {
		src = fmt.Sprintf("https://tranco-list.eu/download/%s/1000000", *id)
	}

	if src == "" {
		log.Fatal("provide -id <tranco-list-id> or -url <zip-url>; " +
			"see https://tranco-list.eu/ for a permanent list ID")
	}

	domains, err := fetchTranco(src)
	if err != nil {
		log.Fatalf("fetch tranco: %v", err)
	}

	if err := writeGzip(*out, domains); err != nil {
		log.Fatalf("write gzip: %v", err)
	}

	sidecar := fmt.Sprintf("source: %s\nid: %s\nfetched: %s\ncount: %d\n",
		src, *id, time.Now().UTC().Format(time.RFC3339), len(domains))
	if err := os.WriteFile(*out+".source", []byte(sidecar), 0o644); err != nil { //nolint:gosec,mnd
		log.Fatalf("write sidecar: %v", err)
	}

	log.Printf("wrote %d domains to %s", len(domains), *out)
}

func fetchTranco(url string) ([]string, error) {
	//nolint:noctx,gosec // one-shot dev tool
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Tranco serves a zip containing top-1m.csv ("rank,domain" lines).
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("not a zip (Tranco download expected): %w", err)
	}

	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".csv") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		return parseCSV(rc)
	}

	return nil, fmt.Errorf("no csv found in tranco zip")
}

func parseCSV(r io.Reader) ([]string, error) {
	var domains []string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// "rank,domain"
		if i := strings.IndexByte(line, ','); i >= 0 {
			line = line[i+1:]
		}

		if d := normalize(line); d != "" {
			domains = append(domains, d)
		}
	}

	return domains, scanner.Err()
}

// normalize lowercases, strips a trailing dot, and drops obviously-invalid
// entries (empty, no dot, whitespace, or leading/trailing dot).
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")

	if s == "" || !strings.Contains(s, ".") || strings.ContainsAny(s, " \t") {
		return ""
	}

	if strings.HasPrefix(s, ".") {
		return ""
	}

	return s
}

func writeGzip(path string, domains []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	gw.Comment = "Tranco top-1m decoy list (see .source sidecar)"

	w := bufio.NewWriter(gw)
	for _, d := range domains {
		if _, err := w.WriteString(d + "\n"); err != nil {
			return err
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}

	return gw.Close()
}
