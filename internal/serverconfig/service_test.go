// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package serverconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestNameDefaultsAndExplicitValues(t *testing.T) {
	for _, test := range []struct{ deployment, namespace, document, want string }{
		{"selfhosted", "production", `{}`, "FydeOS Notes"},
		{"affine", "production", `{}`, "FydeOS Notes Cloud"},
		{"affine", "dev", `{}`, "FydeOS Notes Cloud"},
		{"affine", "beta", `{}`, "FydeOS Notes Cloud"},
		{"selfhosted", "dev", `{"server":{"name":null}}`, "FydeOS Notes"},
		{"affine", "dev", `{"server":{"name":"AFFiNE Canary Cloud"}}`, "FydeOS Notes Cloud"},
		{"selfhosted", "production", `{"server":{"name":"AFFiNE Self-hosted"}}`, "FydeOS Notes"},
		{"affine", "production", `{"server":{"name":""}}`, ""},
		{"affine", "production", `{"server":{"name":" Notes 测试 "}}`, " Notes 测试 "},
	} {
		t.Run(test.deployment+"/"+test.namespace+"/"+test.document, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.document), 0600); err != nil {
				t.Fatal(err)
			}
			s := New(config.Config{Deployment: test.deployment, Namespace: test.namespace, RuntimeConfigPath: path}, nil)
			got, err := s.Name(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("name=%q err=%v want=%q", got, err, test.want)
			}
		})
	}
}

func TestUnconfiguredDatabaseDoesNotReportInitialized(t *testing.T) {
	got, err := New(config.Config{}, nil).Initialized(context.Background())
	if got || err == nil {
		t.Fatalf("initialized without database = %v %v", got, err)
	}
}
