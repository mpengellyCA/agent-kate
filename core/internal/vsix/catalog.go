package vsix

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var catalogJSON []byte

// CatalogEntry is one curated, popular extension users can install with a
// single click from the UI without having to know its Open VSX id.
type CatalogEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Summary     string `json:"summary"`
	Category    string `json:"category"`
}

var catalog = mustLoadCatalog()

func mustLoadCatalog() []CatalogEntry {
	var out []CatalogEntry
	if err := json.Unmarshal(catalogJSON, &out); err != nil {
		panic(fmt.Sprintf("vsix: embedded catalog is invalid: %v", err))
	}
	return out
}

// Catalog returns the curated list of popular extensions, in display order.
// The slice is a copy so callers may sort or filter freely.
func Catalog() []CatalogEntry {
	out := make([]CatalogEntry, len(catalog))
	copy(out, catalog)
	return out
}
