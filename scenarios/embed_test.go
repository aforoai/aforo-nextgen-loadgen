package scenarios

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

func TestNames_AlphabeticalAndYamlOnly(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("Names() returned empty; embed must include scenario YAMLs")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Errorf("Names() not sorted at index %d: %v", i, names)
			break
		}
	}
	for _, n := range names {
		if n == "" {
			t.Error("Names() contains empty entry")
		}
	}
}

func TestRead_KnownAndUnknown(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Skip("no scenarios bundled")
	}
	data, err := Read(names[0])
	if err != nil {
		t.Fatalf("Read(%q) returned error: %v", names[0], err)
	}
	if len(data) == 0 {
		t.Errorf("Read(%q) returned empty bytes", names[0])
	}

	if _, err := Read("does-not-exist-anywhere"); err == nil {
		t.Error("Read(missing): want error, got nil")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read(missing): err is %v; want fs.ErrNotExist", err)
	}
}

func TestRead_OmitsExtension(t *testing.T) {
	// The Names() output must not include the .yaml suffix; Read appends it
	// internally. Reading every name with the bare basename should succeed.
	for _, n := range Names() {
		if strings.HasSuffix(n, ".yaml") {
			t.Errorf("Names() should strip .yaml suffix; got %q", n)
		}
		data, err := Read(n)
		if err != nil {
			t.Errorf("Read(%q): %v", n, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("Read(%q): empty bytes", n)
		}
	}
}
