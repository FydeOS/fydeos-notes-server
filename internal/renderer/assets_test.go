// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package renderer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadAssetsUsesManifestPathsAndSelfHostedRoot(t *testing.T) {
	root := t.TempDir()
	writeManifest := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(filepath.Join(root, "assets-manifest.json"), `{"css":["main.css"],"js":["main.js"],"publicPath":"https://cdn.example/","gitHash":"abc123","description":"Notes"}`)
	writeManifest(filepath.Join(root, "mobile", "assets-manifest.json"), `{"css":["mobile.css"],"js":["mobile.js"],"publicPath":"https://mobile.example/","gitHash":"mobile","description":"Mobile"}`)

	web, mobile, err := LoadAssets(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(web.CSS, []string{"https://cdn.example/main.css"}) || !reflect.DeepEqual(web.JS, []string{"https://cdn.example/main.js"}) || web.GitHash != "abc123" {
		t.Fatalf("web assets = %+v", web)
	}
	if !reflect.DeepEqual(mobile.CSS, []string{"https://mobile.example/mobile.css"}) || mobile.GitHash != "mobile" {
		t.Fatalf("mobile assets = %+v", mobile)
	}
	web, mobile, err = LoadAssets(root, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if web.PublicPath != "/" || !reflect.DeepEqual(web.CSS, []string{"/main.css"}) || mobile.PublicPath != "/" || !reflect.DeepEqual(mobile.JS, []string{"/mobile.js"}) {
		t.Fatalf("self-hosted assets = %+v / %+v", web, mobile)
	}
}

func TestLoadAssetsFailsClosedForMissingProductionManifest(t *testing.T) {
	if _, _, err := LoadAssets(t.TempDir(), false, true); err == nil {
		t.Fatal("production renderer accepted missing asset manifests")
	}
	web, mobile, err := LoadAssets(t.TempDir(), false, false)
	if err != nil || web.PublicPath != "/" || mobile.PublicPath != "/" {
		t.Fatalf("development defaults = %+v / %+v, %v", web, mobile, err)
	}
}
