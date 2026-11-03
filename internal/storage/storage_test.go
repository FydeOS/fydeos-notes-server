// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestLoadScopeConfigsAppliesLegacyEnvironmentThenExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"storages":{"blob.storage":{"provider":"fs","bucket":"file-blobs","config":{"path":"/tmp/file-storage"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"R2_OBJECT_STORAGE_ACCOUNT_ID":        "account-1",
		"R2_OBJECT_STORAGE_ACCESS_KEY_ID":     "r2-access",
		"R2_OBJECT_STORAGE_SECRET_ACCESS_KEY": "r2-secret",
	}
	configs, err := loadScopeConfigs(path, func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if configs[BlobScope].Provider != "fs" || configs[BlobScope].Bucket != "file-blobs" {
		t.Fatalf("explicit blob config did not override legacy environment: %+v", configs[BlobScope])
	}
	if configs[CopilotScope].Provider != "cloudflare-r2" || configs[CopilotScope].Bucket != "notes" || configs[CopilotScope].Endpoint != "https://account-1.r2.cloudflarestorage.com" {
		t.Fatalf("legacy R2 copilot config = %+v", configs[CopilotScope])
	}
	if configs[AvatarScope].Provider != "fs" || configs[AvatarScope].Bucket != "avatars" {
		t.Fatalf("avatar defaults changed: %+v", configs[AvatarScope])
	}
}

func TestLegacyStorageConfigurationRejectsPartialAndAmbiguousCredentials(t *testing.T) {
	for _, values := range []map[string]string{
		{"OSS_ACCESS_KEY_ID": "access"},
		{
			"R2_OBJECT_STORAGE_ACCOUNT_ID": "account", "R2_OBJECT_STORAGE_ACCESS_KEY_ID": "access",
			"R2_OBJECT_STORAGE_SECRET_ACCESS_KEY": "secret", "OSS_ACCESS_KEY_ID": "oss-access",
		},
	} {
		if _, err := legacyStorageConfigs(func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}); err == nil {
			t.Fatalf("legacy config unexpectedly accepted: %#v", values)
		}
	}
}

func TestAssetpackAvatarStorageUsesLocalObjectBackend(t *testing.T) {
	backend, err := newBackend(context.Background(), ScopeConfig{Provider: "assetpack", Bucket: "avatars", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	objects, ok := backend.(ObjectStore)
	if !ok {
		t.Fatalf("assetpack backend %T does not provide object reads", backend)
	}
	length := int64(len("avatar"))
	if _, err := objects.PutObject(context.Background(), "user-avatar", strings.NewReader("avatar"), PutOptions{ContentType: "image/webp", ContentLength: &length}); err != nil {
		t.Fatal(err)
	}
	body, metadata, err := objects.GetObject(context.Background(), "user-avatar")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "avatar" || metadata.ContentType != "image/webp" {
		t.Fatalf("assetpack avatar = %q, %+v, %v", data, metadata, err)
	}
}

func TestFilesystemDeletePrefixIsSegmentBoundedAndRemovesMetadata(t *testing.T) {
	root := t.TempDir()
	bucket := filepath.Join(root, "blobs")
	for name, body := range map[string]string{
		"workspace-a/file.bin":       "target",
		"workspace-a/deep/other.bin": "nested target",
		"workspace-a-extra/file.bin": "neighbor",
		"workspace-b/file.bin":       "unrelated",
	} {
		path := filepath.Join(bucket, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".metadata.json", []byte(`{"contentType":"application/octet-stream","contentLength":1,"lastModified":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend := &filesystemBackend{root: root, bucket: "blobs"}
	deleted, err := backend.DeletePrefix(context.Background(), "workspace-a/")
	if err != nil || deleted != 2 {
		t.Fatalf("delete prefix = %d, %v; want 2 objects", deleted, err)
	}
	for _, name := range []string{"workspace-a-extra/file.bin", "workspace-b/file.bin"} {
		if _, err := os.Stat(filepath.Join(bucket, filepath.FromSlash(name))); err != nil {
			t.Fatalf("nonmatching object %q was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(bucket, "workspace-a/file.bin.metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("object metadata remained after deletion: %v", err)
	}
	if _, err := backend.DeletePrefix(context.Background(), "../"); err == nil {
		t.Fatal("path traversal prefix was accepted")
	}
}

func TestS3DeletePrefixListsAndDeletesMatchingObjects(t *testing.T) {
	var listRequests, deleteRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("S3 request was not signed")
		}
		switch r.Method {
		case http.MethodGet:
			listRequests++
			if r.URL.Path != "/notes" || r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "workspace-1/" {
				t.Errorf("unexpected S3 list request: %s %s", r.Method, r.URL.String())
			}
			w.Header().Set("Content-Type", "application/xml")
			if listRequests == 1 {
				_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>notes</Name><Prefix>workspace-1/</Prefix><KeyCount>1</KeyCount><IsTruncated>false</IsTruncated><Contents><Key>workspace-1/doc.bin</Key><Size>4</Size></Contents></ListBucketResult>`)
			} else {
				_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>notes</Name><Prefix>workspace-1/</Prefix><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
			}
		case http.MethodPost:
			deleteRequests++
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "workspace-1/doc.bin") {
				t.Errorf("S3 delete request did not contain listed key: %s", body)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>workspace-1/doc.bin</Key></Deleted></DeleteResult>`)
		default:
			t.Errorf("unexpected S3 method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	awsCfg := aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", ""),
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	backend := &s3Backend{client: client, bucket: "notes"}
	deleted, err := backend.DeletePrefix(context.Background(), "workspace-1/")
	if err != nil || deleted != 1 {
		t.Fatalf("S3 DeletePrefix = %d, %v; want 1 object", deleted, err)
	}
	if listRequests != 2 || deleteRequests != 1 {
		t.Fatalf("S3 list/delete requests = %d/%d, want 2/1", listRequests, deleteRequests)
	}
}
