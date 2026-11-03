// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestFilesystemObjectRoundTripPreservesSidecarAndDeletesBothFiles(t *testing.T) {
	root := t.TempDir()
	backend := &filesystemBackend{root: root, bucket: "blobs"}
	body := []byte("hello storage")
	contentLength := int64(len(body))
	checksumBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(checksumBytes, crc32.ChecksumIEEE(body))
	checksum := base64.StdEncoding.EncodeToString(checksumBytes)

	metadata, err := backend.PutObject(context.Background(), "workspace-1/blob-1", strings.NewReader(string(body)), PutOptions{
		ContentType: "text/plain", ContentLength: &contentLength, ChecksumCRC32: checksum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ContentType != "text/plain" || metadata.ContentLength != contentLength || metadata.ChecksumCRC32 != checksum || metadata.LastModified.IsZero() {
		t.Fatalf("put metadata = %+v", metadata)
	}
	objectPath := filepath.Join(root, "blobs", "workspace-1", "blob-1")
	stored, err := os.ReadFile(objectPath)
	if err != nil || string(stored) != string(body) {
		t.Fatalf("stored body = %q, %v", stored, err)
	}
	var sidecar filesystemMetadata
	metadataBytes, err := os.ReadFile(objectPath + ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(metadataBytes, &sidecar); err != nil {
		t.Fatal(err)
	}
	if sidecar.ContentType != "text/plain" || sidecar.ContentLength != contentLength || sidecar.LastModified <= 0 || valueString(sidecar.ChecksumCRC32) != checksum {
		t.Fatalf("filesystem metadata sidecar = %+v", sidecar)
	}

	headed, err := backend.HeadObject(context.Background(), "workspace-1/blob-1")
	if err != nil || headed == nil || headed.ChecksumCRC32 != checksum {
		t.Fatalf("head metadata = %+v, %v", headed, err)
	}
	stream, fetched, err := backend.GetObject(context.Background(), "workspace-1/blob-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	fetchedBody, err := io.ReadAll(stream)
	if err != nil || string(fetchedBody) != string(body) || fetched.ContentLength != contentLength {
		t.Fatalf("get object = %q, %+v, %v", fetchedBody, fetched, err)
	}
	rangedBody, err := backend.GetObjectRange(context.Background(), "workspace-1/blob-1", 6, 5)
	if err != nil || string(rangedBody) != "stora" {
		t.Fatalf("get object range = %q, %v; want %q", rangedBody, err, "stora")
	}
	if err := backend.DeleteObject(context.Background(), "workspace-1/blob-1"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{objectPath, objectPath + ".metadata.json"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted path %q still exists: %v", path, err)
		}
	}
	if metadata, err := backend.HeadObject(context.Background(), "workspace-1/blob-1"); err != nil || metadata != nil {
		t.Fatalf("missing head = %+v, %v", metadata, err)
	}
}

func TestFilesystemObjectRejectsMismatchedMetadataAndSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	backend := &filesystemBackend{root: root, bucket: "blobs"}
	body := []byte("content")
	wrongLength := int64(len(body) + 1)
	if _, err := backend.PutObject(context.Background(), "workspace-1/blob-1", strings.NewReader(string(body)), PutOptions{ContentLength: &wrongLength}); err == nil {
		t.Fatal("content length mismatch was accepted")
	}
	if _, err := backend.PutObject(context.Background(), "workspace-1/blob-1", strings.NewReader(string(body)), PutOptions{ChecksumCRC32: "wrong"}); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	if _, err := backend.PutObject(context.Background(), "../outside", strings.NewReader(string(body)), PutOptions{}); err == nil {
		t.Fatal("path traversal was accepted")
	}

	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "blobs", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := backend.PutObject(context.Background(), "linked/object", strings.NewReader(string(body)), PutOptions{}); err == nil {
		t.Fatal("write through storage symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(external, "object")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write escaped storage root: %v", err)
	}
	if _, err := backend.CreateMultipartUpload(context.Background(), "workspace-1/blob-1", PutOptions{}, time.Minute); !errors.Is(err, ErrOperationUnsupported) {
		t.Fatalf("filesystem multipart error = %v", err)
	}
}

func TestS3ObjectAndMultipartOperations(t *testing.T) {
	var partUploads, completes, aborts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("unsigned S3 request %s %s", r.Method, r.URL.String())
		}
		if r.URL.Path != "/notes/workspace-1/blob-1" {
			t.Errorf("unexpected object path %q", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Query().Get("partNumber") == "":
			body, _ := io.ReadAll(r.Body)
			if string(body) != "hello storage" || r.Header.Get("Content-Type") != "text/plain" || r.Header.Get("x-amz-checksum-crc32") == "" {
				t.Errorf("unexpected PutObject request headers/body: %#v %q", r.Header, body)
			}
			w.Header().Set("ETag", `"object-etag"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "13")
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("ETag", `"object-etag"`)
			w.Header().Set("x-amz-checksum-crc32", "checksum")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("uploadId") == "" && r.Header.Get("Range") != "":
			if r.Header.Get("Range") != "bytes=6-10" {
				t.Errorf("object range header = %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "5")
			w.Header().Set("Content-Range", "bytes 6-10/13")
			_, _ = io.WriteString(w, "stora")
		case r.Method == http.MethodGet && r.URL.Query().Get("uploadId") == "":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "13")
			w.Header().Set("ETag", `"object-etag"`)
			_, _ = io.WriteString(w, "hello storage")
		case r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>notes</Bucket><Key>workspace-1/blob-1</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && r.URL.Query().Get("uploadId") == "upload-1":
			partUploads++
			w.Header().Set("ETag", `"part-etag"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("uploadId") == "upload-1":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>notes</Bucket><Key>workspace-1/blob-1</Key><UploadId>upload-1</UploadId><IsTruncated>false</IsTruncated><Part><PartNumber>1</PartNumber><ETag>&quot;part-etag&quot;</ETag><Size>13</Size></Part></ListPartsResult>`)
		case r.Method == http.MethodPost && r.URL.Query().Get("uploadId") == "upload-1":
			completes++
			payload, _ := io.ReadAll(r.Body)
			var body struct {
				XMLName xml.Name `xml:"CompleteMultipartUpload"`
				Parts   []struct {
					PartNumber int    `xml:"PartNumber"`
					ETag       string `xml:"ETag"`
				} `xml:"Part"`
			}
			if err := xml.Unmarshal(payload, &body); err != nil || len(body.Parts) != 2 || body.Parts[0].PartNumber != 1 || body.Parts[1].PartNumber != 2 {
				t.Errorf("completed parts are malformed or unordered: %s (%v)", payload, err)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>notes</Bucket><Key>workspace-1/blob-1</Key><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "upload-1":
			aborts++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected S3 operation: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected operation", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	awsConfig := aws.Config{Region: "auto", Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
		options.HTTPClient = server.Client()
	})
	backend := &s3Backend{client: client, presigner: s3.NewPresignClient(client), bucket: "notes"}
	ctx := context.Background()
	body := []byte("hello storage")
	contentLength := int64(len(body))
	putMetadata, err := backend.PutObject(ctx, "workspace-1/blob-1", strings.NewReader(string(body)), PutOptions{ContentType: "text/plain", ContentLength: &contentLength})
	if err != nil || putMetadata.ETag != `"object-etag"` || putMetadata.ChecksumCRC32 == "" {
		t.Fatalf("PutObject metadata = %+v, %v", putMetadata, err)
	}
	headMetadata, err := backend.HeadObject(ctx, "workspace-1/blob-1")
	if err != nil || headMetadata == nil || headMetadata.ContentLength != contentLength || headMetadata.ContentType != "text/plain" {
		t.Fatalf("HeadObject metadata = %+v, %v", headMetadata, err)
	}
	objectBody, getMetadata, err := backend.GetObject(ctx, "workspace-1/blob-1")
	if err != nil {
		t.Fatal(err)
	}
	readBody, readErr := io.ReadAll(objectBody)
	closeErr := objectBody.Close()
	if readErr != nil || closeErr != nil || string(readBody) != string(body) || getMetadata.ContentLength != contentLength {
		t.Fatalf("GetObject = %q, %+v, read=%v close=%v", readBody, getMetadata, readErr, closeErr)
	}
	rangedBody, err := backend.GetObjectRange(ctx, "workspace-1/blob-1", 6, 5)
	if err != nil || string(rangedBody) != "stora" {
		t.Fatalf("GetObjectRange = %q, %v; want %q", rangedBody, err, "stora")
	}
	if err := backend.DeleteObject(ctx, "workspace-1/blob-1"); err != nil {
		t.Fatal(err)
	}

	init, err := backend.CreateMultipartUpload(ctx, "workspace-1/blob-1", PutOptions{ContentType: "text/plain"}, time.Minute)
	if err != nil || init.UploadID != "upload-1" {
		t.Fatalf("CreateMultipartUpload = %+v, %v", init, err)
	}
	etag, err := backend.UploadPart(ctx, "workspace-1/blob-1", init.UploadID, 1, strings.NewReader("hello storage"), contentLength)
	if err != nil || etag != "part-etag" {
		t.Fatalf("UploadPart = %q, %v", etag, err)
	}
	parts, err := backend.ListMultipartUploadParts(ctx, "workspace-1/blob-1", init.UploadID)
	if err != nil || len(parts) != 1 || parts[0].ETag != "part-etag" || parts[0].Size != contentLength {
		t.Fatalf("ListMultipartUploadParts = %#v, %v", parts, err)
	}
	if err := backend.CompleteMultipartUpload(ctx, "workspace-1/blob-1", init.UploadID, []CompletedPart{
		{PartNumber: 2, ETag: "part-2"}, {PartNumber: 1, ETag: "part-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := backend.AbortMultipartUpload(ctx, "workspace-1/blob-1", init.UploadID); err != nil {
		t.Fatal(err)
	}
	if partUploads != 1 || completes != 1 || aborts != 1 {
		t.Fatalf("multipart operations upload/complete/abort = %d/%d/%d", partUploads, completes, aborts)
	}

	for _, presigned := range []func() (PresignedRequest, error){
		func() (PresignedRequest, error) {
			return backend.PresignPut(ctx, "workspace-1/blob-1", PutOptions{ContentType: "text/plain"}, time.Minute)
		},
		func() (PresignedRequest, error) { return backend.PresignGet(ctx, "workspace-1/blob-1", time.Minute) },
	} {
		request, err := presigned()
		if err != nil || !strings.Contains(request.URL, "X-Amz-Signature=") || request.ExpiresAt.IsZero() {
			t.Fatalf("invalid presigned request %+v: %v", request, err)
		}
	}
	partRequest, err := backend.PresignUploadPart(ctx, "workspace-1/blob-1", "upload-1", 1, contentLength, time.Minute)
	if err != nil || len(partRequest.Headers) != 0 {
		t.Fatalf("multipart presign added request headers not used by the legacy client: %+v, %v", partRequest, err)
	}
	if _, err := backend.PresignPut(ctx, "workspace-1/blob-1", PutOptions{}, 8*24*time.Hour); err == nil {
		t.Fatal("presign expiration above S3 limit was accepted")
	}
}

func TestS3PresignPutHonorsLegacyExpiryAndUnsignedContentTypeSetting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "secret", ""),
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	backend := &s3Backend{
		client: client, presigner: s3.NewPresignClient(client), bucket: "notes",
		presignSignContentTypeSet: true,
	}
	contentLength := int64(17)
	request, err := backend.PresignPut(context.Background(), "workspace-1/blob-1", PutOptions{
		ContentType: "text/plain", ContentLength: &contentLength,
	}, 75*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("X-Amz-Expires") != "75" || strings.Contains(query.Get("X-Amz-SignedHeaders"), "content-type") {
		t.Fatalf("presign URL settings = expires:%q signedHeaders:%q", query.Get("X-Amz-Expires"), query.Get("X-Amz-SignedHeaders"))
	}
	if request.Headers.Get("Content-Type") != "text/plain" || request.Headers.Get("Content-Length") != "17" || request.Headers.Get("Host") != "" {
		t.Fatalf("legacy upload headers = %#v", request.Headers)
	}
}

func TestS3HeadAndGetMapMissingObjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{Region: "auto", Credentials: credentials.NewStaticCredentialsProvider("test", "secret", "")}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
		options.HTTPClient = server.Client()
	})
	backend := &s3Backend{client: client, presigner: s3.NewPresignClient(client), bucket: "notes"}
	if metadata, err := backend.HeadObject(context.Background(), "workspace-1/missing"); err != nil || metadata != nil {
		t.Fatalf("missing HeadObject = %+v, %v", metadata, err)
	}
	if body, _, err := backend.GetObject(context.Background(), "workspace-1/missing"); body != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing GetObject = %v, %v", body, err)
	}
}
