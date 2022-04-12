package search_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/zoekt"
	"github.com/google/zoekt/query"
	"github.com/google/zoekt/web"
	"github.com/sourcegraph/sourcegraph/cmd/searcher/internal/search"
	"github.com/sourcegraph/sourcegraph/cmd/searcher/protocol"
)

func TestHybridSearch(t *testing.T) {
	files := map[string]struct {
		body string
		typ  fileType
	}{
		"README.md": {`# Hello World

Hello world example in go`, typeFile},
		"main.go": {`package main

import "fmt"

func main() {
	fmt.Println("Hello world")
}
`, typeFile},
	}

	// 	protocol.PatternInfo{Pattern: "world"}, `
	// README.md:1:# Hello World
	// README.md:3:Hello world example in go
	// main.go:6:	fmt.Println("Hello world")
	// `},

	s := newStore(t, files)
	ts := httptest.NewServer(&search.Service{Store: s})
	defer ts.Close()

	zoektURL := newZoekt(t, files)

	req := protocol.Request{
		Repo:             "foo",
		URL:              "u",
		Commit:           "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		PatternInfo:      protocol.PatternInfo{Pattern: "world"},
		FetchTimeout:     fetchTimeoutForCI(t),
		IndexerEndpoints: []string{zoektURL},
		FeatHybrid:       true,
	}
	_, err := doSearch(ts.URL, &req)
	if err != nil {
		t.Fatal(err)
	}
}

func newZoekt(t *testing.T, files map[string]struct {
	body string
	typ  fileType
}) string {
	var docs []zoekt.Document
	for name, file := range files {
		docs = append(docs, zoekt.Document{
			Name:    name,
			Content: []byte(file.body),
		})
	}
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].Name < docs[j].Name
	})

	b, err := zoekt.NewIndexBuilder(&zoekt.Repository{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if err := b.Add(d); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	f := &memSeeker{data: buf.Bytes()}

	searcher, err := zoekt.NewSearcher(f)
	if err != nil {
		t.Fatal(err)
	}

	h, err := web.NewMux(&web.Server{
		Searcher: &streamer{Searcher: searcher},
		RPC:      true,
		Top:      web.Top,
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

type streamer struct {
	zoekt.Searcher
}

func (s *streamer) StreamSearch(ctx context.Context, q query.Q, opts *zoekt.SearchOptions, sender zoekt.Sender) (err error) {
	res, err := s.Searcher.Search(ctx, q, opts)
	if err != nil {
		return err
	}
	sender.Send(res)
	return nil
}

type memSeeker struct {
	data []byte
}

func (s *memSeeker) Name() string {
	return "memseeker"
}

func (s *memSeeker) Close() {}
func (s *memSeeker) Read(off, sz uint32) ([]byte, error) {
	return s.data[off : off+sz], nil
}

func (s *memSeeker) Size() (uint32, error) {
	return uint32(len(s.data)), nil
}
