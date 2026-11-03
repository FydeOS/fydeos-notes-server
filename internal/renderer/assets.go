// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package renderer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func LoadAssets(root string, selfHosted, production bool) (Assets, Assets, error) {
	web, err := loadAssetsManifest(filepath.Join(root, "assets-manifest.json"), selfHosted, production)
	if err != nil {
		return Assets{}, Assets{}, err
	}
	mobile, err := loadAssetsManifest(filepath.Join(root, "mobile", "assets-manifest.json"), selfHosted, production)
	if err != nil {
		return Assets{}, Assets{}, err
	}
	return web, mobile, nil
}

func loadAssetsManifest(path string, selfHosted, production bool) (Assets, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if production {
			return Assets{}, fmt.Errorf("read renderer asset manifest: %w", err)
		}
		return defaultAssets(), nil
	}
	var assets Assets
	if err := json.Unmarshal(contents, &assets); err != nil {
		if production {
			return Assets{}, fmt.Errorf("decode renderer asset manifest: %w", err)
		}
		return defaultAssets(), nil
	}
	if selfHosted {
		assets.PublicPath = "/"
	}
	assets.CSS = prependAssetPath(assets.PublicPath, assets.CSS)
	assets.JS = prependAssetPath(assets.PublicPath, assets.JS)
	return assets, nil
}

func defaultAssets() Assets {
	return Assets{CSS: []string{}, JS: []string{}, PublicPath: "/"}
}

func prependAssetPath(publicPath string, assets []string) []string {
	result := make([]string, 0, len(assets))
	for _, asset := range assets {
		result = append(result, publicPath+asset)
	}
	return result
}
