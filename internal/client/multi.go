package client

import (
	"context"
	"sort"
	"sync"

	"super-speedy-search/internal/search"
)

// NodeError reports a per-node failure during a fan-out search; other nodes'
// results are still returned.
type NodeError struct {
	NodeURL string `json:"node_url"`
	Error   string `json:"error"`
}

// FanOutMetadata queries all clients concurrently and returns merged,
// globally ranked results plus per-node errors.
func FanOutMetadata(ctx context.Context, clients map[string]*Client, p search.Params) ([]search.Result, []NodeError) {
	var (
		mu      sync.Mutex
		results []search.Result
		errs    []NodeError
		wg      sync.WaitGroup
	)
	for url, c := range clients {
		wg.Add(1)
		go func(url string, c *Client) {
			defer wg.Done()
			r, err := c.SearchMetadata(ctx, p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, NodeError{NodeURL: url, Error: err.Error()})
				return
			}
			results = append(results, r...)
		}(url, c)
	}
	wg.Wait()
	RankGlobal(results)
	sort.Slice(errs, func(i, j int) bool { return errs[i].NodeURL < errs[j].NodeURL })
	return results, errs
}

// RankGlobal orders merged results from several nodes using the raw match
// signals each node returns: match type first, then recency.
func RankGlobal(results []search.Result) {
	rank := map[string]int{
		search.MatchFilenameExact: 3,
		search.MatchFilename:      2,
		search.MatchPath:          1,
	}
	sort.SliceStable(results, func(i, j int) bool {
		if rank[results[i].MatchType] != rank[results[j].MatchType] {
			return rank[results[i].MatchType] > rank[results[j].MatchType]
		}
		return results[i].ModifiedAt > results[j].ModifiedAt
	})
}
