// Package seed: listpage.go provides helpers for decoding Aforo list/page
// responses into a flat []T. Centralizes the two-layer envelope handling that
// every lookup function needs:
//
//  1. ApiResponseAdvice envelope: {"success":true,"data":<inner>,"meta":...}
//     — already partially handled by client.go's unmarshalAforoResponse
//     but only at the top level.
//  2. Spring Page envelope:        {"content":[...], "totalElements":N, ...}
//     — used by controllers that return Page<T>.
//  3. Plain array:                  [{...}, ...]
//     — used by controllers that return List<T> (e.g. wallet listAll).
//
// listAll handles all three shapes. Callers pass a target slice pointer and
// the helper populates it. Returns the total element count for the caller's
// paging logic (or -1 if the response was a plain array with no count
// metadata).
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// listAll issues a GET against the given URL, transparently unwraps the
// ApiResponse envelope AND the Spring Page<T> envelope, and decodes the inner
// content into out (a pointer to a slice).
//
// Returns:
//   - total: the page's totalElements field if present, or len(out) for plain
//     arrays, or 0 if the response was empty
//   - err:   any HTTP transport or decoding error
//
// Callers that need paging should call this once per page using opts.Query
// to thread ?page=N&size=M. Today the seed harness reads page 0 with default
// size — enough for the lookup-by-name use case because loadgen-generated
// names are unique per (tenant, archetype) so at most one match exists in
// the first page even when the tenant has 10k entities.
//
// Note: this helper does NOT replace c.Do for non-list endpoints. Use it only
// when the backend returns Page<T> or List<T>.
func listAll(ctx context.Context, c *Client, url string, opts RequestOptions, out any) (total int, err error) {
	// Decode into a raw JSON value so we can inspect the shape before
	// committing to a target type.
	var raw json.RawMessage
	if err := c.Do(ctx, http.MethodGet, url, nil, &raw, opts); err != nil {
		return 0, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}

	// Try plain array first — the cheapest path.
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("decode array: %w", err)
		}
		return -1, nil
	}

	// Otherwise it's an object. Could be a Page envelope ({content: [...]})
	// OR a raw ApiResponse envelope ({data: ...}) that the underlying client
	// did NOT unwrap (e.g. because the top-level Unmarshal into json.RawMessage
	// succeeded without unwrapping). Handle both.
	var envelope struct {
		Content       json.RawMessage `json:"content"`
		TotalElements int             `json:"totalElements"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, fmt.Errorf("decode envelope: %w", err)
	}

	// Case A: it's a Page<T> directly (content + totalElements).
	if len(envelope.Content) > 0 {
		if err := json.Unmarshal(envelope.Content, out); err != nil {
			return 0, fmt.Errorf("decode Page.content: %w", err)
		}
		return envelope.TotalElements, nil
	}

	// Case B: it's an ApiResponse envelope wrapping either an array or a
	// Page. Unwrap one more level and recurse on the result.
	if len(envelope.Data) > 0 {
		var inner struct {
			Content       json.RawMessage `json:"content"`
			TotalElements int             `json:"totalElements"`
		}
		// If the data is itself an array, decode directly.
		if envelope.Data[0] == '[' {
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				return 0, fmt.Errorf("decode data array: %w", err)
			}
			return -1, nil
		}
		// Otherwise expect data to be a Page object.
		if err := json.Unmarshal(envelope.Data, &inner); err != nil {
			return 0, fmt.Errorf("decode data envelope: %w", err)
		}
		if len(inner.Content) > 0 {
			if err := json.Unmarshal(inner.Content, out); err != nil {
				return 0, fmt.Errorf("decode data.content: %w", err)
			}
			return inner.TotalElements, nil
		}
	}

	// Unrecognised shape. Don't fail — return an empty result so the caller's
	// "not found" branch fires. The backend may have returned an unexpected
	// envelope (e.g. {error: ...} without an HTTP error status), which would
	// be a separate bug to surface — but it shouldn't crash loadgen.
	return 0, nil
}

// listAllOptional wraps listAll and swallows 404 → ([], 0, nil). Use for
// lookup-before-create flows where "not found" is a natural outcome.
func listAllOptional(ctx context.Context, c *Client, url string, opts RequestOptions, out any) (total int, err error) {
	total, err = listAll(ctx, c, url, opts, out)
	if err != nil && aforo.IsNotFound(err) {
		return 0, nil
	}
	return total, err
}
