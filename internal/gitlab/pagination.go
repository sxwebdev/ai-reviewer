package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// maxListPages bounds pagination of any list endpoint. At per_page=100 that is
// 20 000 items, well past anything a real project produces, and it is the same
// bound slack.ListUsers uses for the same reason: every page is accumulated in
// one slice and a GitLab response may be up to 32 MiB, so an endpoint that
// keeps handing out an X-Next-Page can walk a worker's RSS into the pod's
// memory limit and OOMKill the replica mid-review.
//
// Exceeding it is an error rather than a truncation. A short list that looks
// complete is the worse failure: ListOpenMRs would silently stop reviewing
// everything past the cut, with nothing to notice it by.
const maxListPages = 200

// getList GETs all pages of a list endpoint, following the X-Next-Page header.
func getList[T any](ctx context.Context, t *transport, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "100")

	page := 1
	var out []T
	for range maxListPages {
		query.Set("page", strconv.Itoa(page))
		resp, err := t.doRaw(ctx, "GET", path, query, nil)
		if err != nil {
			return nil, err
		}
		if len(resp.body) > 0 {
			var batch []T
			if err := json.Unmarshal(resp.body, &batch); err != nil {
				return nil, fmt.Errorf("decode page %d of %s: %w", page, path, err)
			}
			out = append(out, batch...)
		}
		next := resp.header.Get("X-Next-Page")
		if next == "" || next == "0" {
			return out, nil
		}
		p, err := strconv.Atoi(next)
		if err != nil || p <= page {
			return out, nil
		}
		page = p
	}
	return nil, fmt.Errorf("%s: pagination did not terminate after %d pages", path, maxListPages)
}
