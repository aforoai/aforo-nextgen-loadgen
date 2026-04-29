package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Document is a loaded scenario together with the raw YAML node tree. The
// node tree is retained so validation errors can resolve a logical path
// (e.g. "tenants.archetypes[3].weight") to a file:line:col location.
type Document struct {
	Scenario *Scenario
	Root     *yaml.Node
	Path     string
}

// LoadFromFile reads the file at path, decodes it into a Scenario, applies
// defaults, and returns a Document. Validation is the caller's responsibility
// — call Validate(doc) on the returned value.
//
// The strictness contract: KnownFields(true) — unknown top-level or nested
// keys are an error. This catches typos like `target_tps_` or `tenant` (sing.).
func LoadFromFile(path string) (*Document, error) {
	data, err := os.ReadFile(path) // #nosec G304 — caller-controlled path
	if err != nil {
		return nil, fmt.Errorf("read scenario %q: %w", path, err)
	}
	doc, err := LoadFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("load %q: %w", path, err)
	}
	doc.Path = path
	return doc, nil
}

// LoadFromBytes is the in-memory variant — useful for embedded scenarios
// and tests. Path is empty on the returned Document; callers can set it.
func LoadFromBytes(data []byte) (*Document, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("scenario is empty")
	}

	// Decode into a yaml.Node first to capture line/col for validation
	// errors. yaml.v3 fills Line/Column on every node it constructs.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}

	// Strict decode into Scenario. KnownFields(true) rejects unknown keys.
	var s Scenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}

	applyDefaults(&s)

	return &Document{Scenario: &s, Root: &root}, nil
}

// findNode walks the root yaml.Node tree along path and returns the matched
// node, or nil if any segment is missing. Path segments are either map keys
// ("tenants", "archetypes") or array indices in the form "[N]".
//
// This is best-effort: if the path doesn't resolve, validation errors fall
// back to (line=0, col=0) and the message renders without file:line:col.
func findNode(root *yaml.Node, path []string) *yaml.Node {
	if root == nil {
		return nil
	}
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, seg := range path {
		if n == nil {
			return nil
		}
		if isIndexSegment(seg) {
			idx := parseIndexSegment(seg)
			if n.Kind != yaml.SequenceNode || idx < 0 || idx >= len(n.Content) {
				return nil
			}
			n = n.Content[idx]
			continue
		}
		if n.Kind != yaml.MappingNode {
			return nil
		}
		var found *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				found = n.Content[i+1]
				break
			}
		}
		if found == nil {
			return nil
		}
		n = found
	}
	return n
}

// isIndexSegment reports whether seg has the form "[N]".
func isIndexSegment(seg string) bool {
	if len(seg) < 3 || seg[0] != '[' || seg[len(seg)-1] != ']' {
		return false
	}
	for i := 1; i < len(seg)-1; i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return false
		}
	}
	return true
}

// parseIndexSegment turns "[42]" into 42. Caller must verify isIndexSegment.
func parseIndexSegment(seg string) int {
	n := 0
	for i := 1; i < len(seg)-1; i++ {
		n = n*10 + int(seg[i]-'0')
	}
	return n
}
