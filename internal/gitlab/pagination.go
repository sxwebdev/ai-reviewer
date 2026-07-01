package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// getList GETs all pages of a list endpoint, following the X-Next-Page header.
func getList[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "100")

	page := 1
	var out []T
	for {
		query.Set("page", strconv.Itoa(page))
		resp, err := c.doRaw(ctx, "GET", path, query, nil)
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
			break
		}
		p, err := strconv.Atoi(next)
		if err != nil || p <= page {
			break
		}
		page = p
	}
	return out, nil
}
