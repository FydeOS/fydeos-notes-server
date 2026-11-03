// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"sync"
)

//go:embed model_catalog.json
var catalogJSON []byte

var loadCatalog = sync.OnceValues(func() (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(catalogJSON, &catalog); err != nil {
		return Catalog{}, err
	}
	catalog.Version = ""
	encoded, err := json.Marshal(catalog)
	if err != nil {
		return Catalog{}, err
	}
	sum := sha256.Sum256(encoded)
	catalog.Version = hex.EncodeToString(sum[:])[:16]
	return catalog, nil
})

// ByokCatalog returns a copy of the embedded model catalog.
func ByokCatalog() (Catalog, error) {
	catalog, err := loadCatalog()
	if err != nil {
		return Catalog{}, err
	}
	// Deep copy through JSON so callers cannot mutate the shared value.
	encoded, _ := json.Marshal(catalog)
	var copied Catalog
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return Catalog{}, err
	}
	return copied, nil
}

func catalogModel(provider, modelID string) (CatalogModel, bool) {
	catalog, err := loadCatalog()
	if err != nil {
		return CatalogModel{}, false
	}
	for _, p := range catalog.Providers {
		if p.Provider != provider {
			continue
		}
		for _, m := range p.Models {
			if m.ModelID == modelID {
				return m, true
			}
		}
	}
	return CatalogModel{}, false
}

// ModelSupportsServerAction reports whether the catalog capabilities of the
// model satisfy a known server action.
func ModelSupportsServerAction(provider, modelID, operation string) bool {
	requirement, ok := serverRequirements[operation]
	if !ok {
		return false
	}
	model, ok := catalogModel(provider, modelID)
	return ok && satisfies(model.Capabilities, requirement)
}
