package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stateKeyReaderFunc func(context.Context, pluginsdk.StateKeyQuery) (pluginsdk.StateKeyPage, error)

func (f stateKeyReaderFunc) ListKeys(ctx context.Context, query pluginsdk.StateKeyQuery) (pluginsdk.StateKeyPage, error) {
	return f(ctx, query)
}

func (h *fakeHost) StateKeys() pluginsdk.StateKeyReader {
	return stateKeyReaderFunc(h.listStateKeys)
}

// Model the SDK's exclusive keyset contract, including an empty state key.
// The plugin must return the fixture cursor unchanged and keep its query fixed.
func (h *fakeHost) listStateKeys(ctx context.Context, query pluginsdk.StateKeyQuery) (pluginsdk.StateKeyPage, error) {
	if err := ctx.Err(); err != nil {
		return pluginsdk.StateKeyPage{}, err
	}
	if query.Limit == 0 {
		query.Limit = 200
	}
	if query.Limit < 1 || query.Limit > 200 {
		return pluginsdk.StateKeyPage{}, status.Error(codes.InvalidArgument, "invalid key limit")
	}
	type continuation struct {
		Scope, ScopeID string
		Limit          int
		After          string
	}
	var cursor continuation
	if query.Cursor != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(query.Cursor, "keys.fixture+/="))
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.Scope != query.Scope || cursor.ScopeID != query.ScopeID || cursor.Limit != query.Limit {
			return pluginsdk.StateKeyPage{}, status.Error(codes.InvalidArgument, "invalid key cursor or namespace")
		}
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	prefix := query.Scope + "\x00" + query.ScopeID + "\x00"
	keys := make([]string, 0)
	for key := range h.state {
		if strings.HasPrefix(key, prefix) {
			key = strings.TrimPrefix(key, prefix)
			if query.Cursor == "" || key > cursor.After {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	page := pluginsdk.StateKeyPage{Keys: keys}
	if len(keys) > query.Limit {
		page.Keys = keys[:query.Limit]
		encoded, _ := json.Marshal(continuation{query.Scope, query.ScopeID, query.Limit, page.Keys[len(page.Keys)-1]})
		page.NextCursor = "keys.fixture+/=" + base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}
