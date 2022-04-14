package search

import (
	"archive/zip"
	"context"
	"io"
	"regexp/syntax" //nolint:depguard // zoekt requires this pkg
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/zoekt"
	zoektquery "github.com/google/zoekt/query"
	"github.com/grafana/regexp"
	"github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/cmd/searcher/protocol"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/comby"
	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/search/backend"
	zoektutil "github.com/sourcegraph/sourcegraph/internal/search/zoekt"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/trace/ot"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

var (
	zoektOnce   sync.Once
	endpointMap atomicEndpoints
	zoektClient zoekt.Streamer
)

func getZoektClient(indexerEndpoints []string) zoekt.Streamer {
	zoektOnce.Do(func() {
		zoektClient = backend.NewMeteredSearcher(
			"", // no hostname means its the aggregator
			&backend.HorizontalSearcher{
				Map:  &endpointMap,
				Dial: backend.ZoektDial,
			},
		)
	})
	endpointMap.Set(indexerEndpoints)
	return zoektClient
}

func handleFilePathPatterns(query *search.TextPatternInfo) (zoektquery.Q, error) {
	var and []zoektquery.Q

	// Zoekt uses regular expressions for file paths.
	// Unhandled cases: PathPatternsAreCaseSensitive and whitespace in file path patterns.
	for _, p := range query.IncludePatterns {
		q, err := zoektutil.FileRe(p, query.IsCaseSensitive)
		if err != nil {
			return nil, err
		}
		and = append(and, q)
	}
	if query.ExcludePattern != "" {
		q, err := zoektutil.FileRe(query.ExcludePattern, query.IsCaseSensitive)
		if err != nil {
			return nil, err
		}
		and = append(and, &zoektquery.Not{Child: q})
	}

	// For conditionals that happen on a repo we can use type:repo queries. eg
	// (type:repo file:foo) (type:repo file:bar) will match all repos which
	// contain a filename matching "foo" and a filename matchinb "bar".
	//
	// Note: (type:repo file:foo file:bar) will only find repos with a
	// filename containing both "foo" and "bar".
	for _, p := range query.FilePatternsReposMustInclude {
		q, err := zoektutil.FileRe(p, query.IsCaseSensitive)
		if err != nil {
			return nil, err
		}
		and = append(and, &zoektquery.Type{Type: zoektquery.TypeRepo, Child: q})
	}
	for _, p := range query.FilePatternsReposMustExclude {
		q, err := zoektutil.FileRe(p, query.IsCaseSensitive)
		if err != nil {
			return nil, err
		}
		and = append(and, &zoektquery.Not{Child: &zoektquery.Type{Type: zoektquery.TypeRepo, Child: q}})
	}

	return zoektquery.NewAnd(and...), nil
}

func buildQuery(args *search.TextPatternInfo, branchRepos []zoektquery.BranchRepos, filePathPatterns zoektquery.Q, shortcircuit bool) (zoektquery.Q, error) {
	regexString := comby.StructuralPatToRegexpQuery(args.Pattern, shortcircuit)
	if len(regexString) == 0 {
		return &zoektquery.Const{Value: true}, nil
	}
	re, err := syntax.Parse(regexString, syntax.ClassNL|syntax.PerlX|syntax.UnicodeGroups)
	if err != nil {
		return nil, err
	}
	return zoektquery.NewAnd(
		&zoektquery.BranchesRepos{List: branchRepos},
		filePathPatterns,
		&zoektquery.Regexp{
			Regexp:        re,
			CaseSensitive: true,
			Content:       true,
		},
	), nil
}

type zoektSearchStreamEvent struct {
	fm       []zoekt.FileMatch
	limitHit bool
	partial  map[api.RepoID]struct{}
	err      error
}

const defaultMaxSearchResults = 30

// zoektSearch searches repositories using zoekt, returning file contents for
// files that match the given pattern.
//
// Timeouts are reported through the context, and as a special case errNoResultsInTimeout
// is returned if no results are found in the given timeout (instead of the more common
// case of finding partial or full results in the given timeout).
func zoektSearch(ctx context.Context, args *search.TextPatternInfo, branchRepos []zoektquery.BranchRepos, since func(t time.Time) time.Duration, endpoints []string, useFullDeadline bool, c chan<- zoektSearchStreamEvent) (fm []zoekt.FileMatch, limitHit bool, partial map[api.RepoID]struct{}, err error) {
	defer func() {
		if c != nil {
			c <- zoektSearchStreamEvent{
				fm:       fm,
				limitHit: limitHit,
				partial:  partial,
				err:      err,
			}
		}
	}()
	if len(branchRepos) == 0 {
		return nil, false, nil, nil
	}

	numRepos := 0
	for _, br := range branchRepos {
		numRepos += int(br.Repos.GetCardinality())
	}

	// Choose sensible values for k when we generalize this.
	k := zoektutil.ResultCountFactor(numRepos, args.FileMatchLimit, false)
	searchOpts := zoektutil.SearchOpts(ctx, k, args.FileMatchLimit, nil)
	searchOpts.Whole = true

	// TODO(@camdencheek) TODO(@rvantonder) handle "timeout:..." values in this context.
	if useFullDeadline {
		// If the user manually specified a timeout, allow zoekt to use all of the remaining timeout.
		deadline, _ := ctx.Deadline()
		searchOpts.MaxWallTime = time.Until(deadline)

		// We don't want our context's deadline to cut off zoekt so that we can get the results
		// found before the deadline.
		//
		// We'll create a new context that gets cancelled if the other context is cancelled for any
		// reason other than the deadline being exceeded. This essentially means the deadline for the new context
		// will be `deadline + time for zoekt to cancel + network latency`.
		var cancel context.CancelFunc
		ctx, cancel = contextWithoutDeadline(ctx)
		defer cancel()
	}

	filePathPatterns, err := handleFilePathPatterns(args)
	if err != nil {
		return nil, false, nil, err
	}

	t0 := time.Now()
	q, err := buildQuery(args, branchRepos, filePathPatterns, true)
	if err != nil {
		return nil, false, nil, err
	}

	client := getZoektClient(endpoints)
	resp, err := client.Search(ctx, q, &searchOpts)
	if err != nil {
		return nil, false, nil, err
	}
	if since(t0) >= searchOpts.MaxWallTime {
		return nil, false, nil, errNoResultsInTimeout
	}

	// We always return approximate results (limitHit true) unless we run the branch to perform a more complete search.
	limitHit = true
	// If the previous indexed search did not return a substantial number of matching file candidates or count was
	// manually specified, run a more complete and expensive search.
	if resp.FileCount < 10 || args.FileMatchLimit != defaultMaxSearchResults {
		q, err = buildQuery(args, branchRepos, filePathPatterns, false)
		if err != nil {
			return nil, false, nil, err
		}
		resp, err = client.Search(ctx, q, &searchOpts)
		if err != nil {
			return nil, false, nil, err
		}
		if since(t0) >= searchOpts.MaxWallTime {
			return nil, false, nil, errNoResultsInTimeout
		}
		// This is the only place limitHit can be set false, meaning we covered everything.
		limitHit = resp.FilesSkipped+resp.ShardsSkipped > 0
	}

	if len(resp.Files) == 0 {
		return nil, false, nil, nil
	}

	maxLineMatches := 25 + k
	for _, file := range resp.Files {
		if len(file.LineMatches) > maxLineMatches {
			file.LineMatches = file.LineMatches[:maxLineMatches]
			limitHit = true
		}
	}

	return resp.Files, limitHit, partial, nil
}

// zoektCompile builds a text search zoekt query for p.
//
// This function should support the same features as the "compile" function,
// but return a zoektquery instead of a readerGrep.
//
// Note: This is used by hybrid search and not structural search.
func zoektCompile(p *protocol.PatternInfo) (zoektquery.Q, error) {
	var parts []zoektquery.Q
	// we are redoing work here, but ensures we generate the same regex and it
	// feels nicer than passing in a readerGrep since handle path directly.
	if rg, err := compile(p); err != nil {
		return nil, err
	} else {
		re, err := syntax.Parse(rg.re.String(), syntax.Perl)
		if err != nil {
			return nil, err
		}
		parts = append(parts, &zoektquery.Regexp{
			Regexp:        re,
			Content:       true,
			CaseSensitive: !rg.ignoreCase,
		})
	}

	for _, pat := range p.IncludePatterns {
		if !p.PathPatternsAreRegExps {
			return nil, errors.New("hybrid search expects PathPatternsAreRegExps")
		}
		re, err := syntax.Parse(pat, syntax.Perl)
		if err != nil {
			return nil, err
		}
		parts = append(parts, &zoektquery.Regexp{
			Regexp:        re,
			FileName:      true,
			CaseSensitive: p.PathPatternsAreCaseSensitive,
		})
	}

	if p.ExcludePattern != "" {
		if !p.PathPatternsAreRegExps {
			return nil, errors.New("hybrid search expects PathPatternsAreRegExps")
		}
		re, err := syntax.Parse(p.ExcludePattern, syntax.Perl)
		if err != nil {
			return nil, err
		}
		parts = append(parts, &zoektquery.Not{Child: &zoektquery.Regexp{
			Regexp:        re,
			FileName:      true,
			CaseSensitive: p.PathPatternsAreCaseSensitive,
		}})
	}

	return zoektquery.Simplify(zoektquery.NewAnd(parts...)), nil
}

func zoektIgnorePaths(paths []string) zoektquery.Q {
	if len(paths) == 0 {
		return &zoektquery.Const{Value: true}
	}

	parts := make([]zoektquery.Q, 0, len(paths))
	for _, p := range paths {
		re, err := syntax.Parse("^"+regexp.QuoteMeta(p)+"$", syntax.Perl)
		if err != nil {
			panic("failed to regex compile escaped literal: " + err.Error())
		}
		parts = append(parts, &zoektquery.Regexp{
			Regexp:        re,
			FileName:      true,
			CaseSensitive: true,
		})
	}

	return &zoektquery.Not{Child: zoektquery.NewOr(parts...)}
}

// zoektIndexedCommit returns the default indexed commit for a repository.
func zoektIndexedCommit(ctx context.Context, endpoints []string, repo api.RepoName) (api.CommitID, bool, error) {
	// TODO check we are using the most efficient way to List. I tested with
	// NewSingleBranchesRepos and it went through a slow path.
	q := zoektquery.NewRepoSet(string(repo))

	client := getZoektClient(endpoints)
	resp, err := client.List(ctx, q, &zoekt.ListOptions{Minimal: true})
	if err != nil {
		return "", false, err
	}

	for _, v := range resp.Minimal {
		return api.CommitID(v.Branches[0].Version), true, nil
	}

	return "", false, nil
}

func writeZip(ctx context.Context, w io.Writer, fileMatches []zoekt.FileMatch) (err error) {
	bytesWritten := 0
	span, _ := ot.StartSpanFromContext(ctx, "WriteZip")
	defer func() {
		span.LogFields(log.Int("bytes_written", bytesWritten))
		span.Finish()
	}()

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, match := range fileMatches {
		mw, err := zw.Create(match.FileName)
		if err != nil {
			return err
		}

		n, err := mw.Write(match.Content)
		if err != nil {
			return err
		}
		bytesWritten += n
	}

	return nil
}

var errNoResultsInTimeout = errors.New("no results found in specified timeout")

// contextWithoutDeadline returns a context which will cancel if the cOld is
// canceled.
func contextWithoutDeadline(cOld context.Context) (context.Context, context.CancelFunc) {
	cNew, cancel := context.WithCancel(context.Background())

	// Set trace context so we still get spans propagated
	cNew = trace.CopyContext(cNew, cOld)

	// Copy actor from cOld to cNew.
	cNew = actor.WithActor(cNew, actor.FromContext(cOld))

	go func() {
		select {
		case <-cOld.Done():
			// cancel the new context if the old one is done for some reason other than the deadline passing.
			if cOld.Err() != context.DeadlineExceeded {
				cancel()
			}
		case <-cNew.Done():
		}
	}()

	return cNew, cancel
}

// atomicEndpoints allows us to update the endpoints used by our zoekt client.
type atomicEndpoints struct {
	endpoints atomic.Value
}

func (a *atomicEndpoints) Endpoints() ([]string, error) {
	eps := a.endpoints.Load()
	if eps == nil {
		return nil, errors.New("endpoints have not been set")
	}
	return eps.([]string), nil
}

func (a *atomicEndpoints) Set(endpoints []string) {
	if !a.needsUpdate(endpoints) {
		return
	}
	a.endpoints.Store(endpoints)
}

func (a *atomicEndpoints) needsUpdate(endpoints []string) bool {
	old, err := a.Endpoints()
	if err != nil {
		return true
	}
	if len(old) != len(endpoints) {
		return true
	}

	for i := range endpoints {
		if old[i] != endpoints[i] {
			return true
		}
	}

	return false
}
