package localmail

import "github.com/nimysan/mcp-hub/internal/localmail/hxstore"

func hxErrUnsupported() error { return hxstore.ErrUnsupportedFormat }
func hxErrNotFound() error    { return hxstore.ErrNotFound }
func hxSummary(inc ...string) hxstore.Summary {
	return hxstore.Summary{ID: 1, Incomplete: inc}
}
