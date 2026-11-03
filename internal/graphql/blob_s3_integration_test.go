// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobmanifesthttp"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The blob upload operations below mirror the documents sent by the web
// client for these schema fields.
var liveS3Operations = map[string]string{
	"blob-upload-create.gql": `mutation createBlobUpload($workspaceId: String!, $key: String!, $size: Int!, $mime: String!) {
  createBlobUpload(workspaceId: $workspaceId, key: $key, size: $size, mime: $mime) {
    method
    blobKey
    alreadyUploaded
    uploadUrl
    headers
    expiresAt
    uploadId
    partSize
    uploadedParts { partNumber etag }
  }
}`,
	"blob-upload-complete.gql": `mutation completeBlobUpload($workspaceId: String!, $key: String!, $uploadId: String, $parts: [BlobUploadPartInput!]) {
  completeBlobUpload(workspaceId: $workspaceId, key: $key, uploadId: $uploadId, parts: $parts)
}`,
	"blob-upload-abort.gql": `mutation abortBlobUpload($workspaceId: String!, $key: String!, $uploadId: String!) {
  abortBlobUpload(workspaceId: $workspaceId, key: $key, uploadId: $uploadId)
}`,
	"blob-upload-part-url.gql": `query getBlobUploadPartUrl($workspaceId: String!, $key: String!, $uploadId: String!, $partNumber: Int!) {
  workspace(id: $workspaceId) {
    blobUploadPartUrl(key: $key, uploadId: $uploadId, partNumber: $partNumber) {
      uploadUrl
      headers
      expiresAt
    }
  }
}`,
	"blob-delete.gql": `mutation deleteBlob($workspaceId: String!, $key: String!, $permanently: Boolean!) {
  deleteBlob(workspaceId: $workspaceId, key: $key, permanently: $permanently)
}`,
}

// This test uses an independently running S3 service, PostgreSQL, real cookie
// authentication and the client's GraphQL operations. Neither S3 nor the
// upload service is replaced by a fake.
func TestLiveS3GraphQLBlobUploadLifecycle(t *testing.T) {
	endpoint := os.Getenv("NOTES_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set NOTES_TEST_S3_ENDPOINT to a disposable loopback S3 service")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" && parsed.Hostname() != "localhost") {
		t.Fatal("live S3 tests require a loopback HTTP origin")
	}
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			h := newLiveS3UploadFixture(t, endpoint, mode == "proxy")
			ctx := t.Context()
			body := []byte("single object through the current client GraphQL operation")
			key := liveS3ContentKey(body)
			createVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": key, "size": len(body), "mime": "text/plain"}
			if result := h.graph(t, "blob-upload-create.gql", createVars, ""); result["errors"] == nil {
				t.Fatal("anonymous reservation succeeded")
			}
			createVars["workspaceId"] = h.fixture.hiddenWorkspaceID
			if result := h.graph(t, "blob-upload-create.gql", createVars, h.fixture.userID); result["errors"] == nil {
				t.Fatal("cross-workspace reservation succeeded")
			}
			h.assertLedgerCount(t, 0)
			h.assertObjectCount(t, 0)
			createVars["workspaceId"] = h.fixture.workspaceID
			init := liveS3Data(t, h.graph(t, "blob-upload-create.gql", createVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			if init["method"] != "PRESIGNED" {
				t.Fatalf("single upload method=%v", init["method"])
			}
			badURL, err := url.Parse(init["uploadUrl"].(string))
			if err != nil {
				t.Fatal(err)
			}
			query := badURL.Query()
			if mode == "direct" {
				query.Set("X-Amz-Signature", strings.Repeat("0", 64))
			} else {
				query.Set("token", "invalid-signature")
			}
			badURL.RawQuery = query.Encode()
			bad := map[string]any{"uploadUrl": badURL.String(), "headers": init["headers"]}
			if status, _ := h.put(t, bad, body); status < 400 {
				t.Fatalf("tampered upload signature returned %d", status)
			}
			h.assertObjectCount(t, 0)
			h.putOK(t, init, body)
			completeVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": key}
			if got := liveS3Data(t, h.graph(t, "blob-upload-complete.gql", completeVars, h.fixture.userID), "completeBlobUpload"); got != key {
				t.Fatalf("completed single key=%v", got)
			}
			h.assertCompleted(t, key, body)
			rootDoc := ygo.NewDoc()
			meta := ygo.NewMap(rootDoc, "meta")
			txn := rootDoc.WriteTxn()
			meta.Set(txn, "avatar", key)
			txn.Commit()
			if _, err := h.pool.Exec(ctx, `UPDATE snapshots SET blob=$2,updated_at=clock_timestamp() WHERE workspace_id=$1 AND guid=$1`, h.fixture.workspaceID, ygo.EncodeStateAsUpdate(rootDoc)); err != nil {
				t.Fatal(err)
			}
			h.downloadS3Blob(t, key, body)
			duplicate := liveS3Data(t, h.graph(t, "blob-upload-create.gql", createVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			if duplicate["alreadyUploaded"] != true {
				t.Fatal("completed object was reserved twice")
			}

			large := append(bytes.Repeat([]byte("a"), 5<<20), bytes.Repeat([]byte("b"), 5<<20)...)
			largeKey := liveS3ContentKey(large)
			largeVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": largeKey, "size": len(large), "mime": "application/octet-stream"}
			multipart := liveS3Data(t, h.graph(t, "blob-upload-create.gql", largeVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			if multipart["method"] != "MULTIPART" || multipart["partSize"] != float64(5<<20) {
				t.Fatalf("multipart method/size=%v/%v", multipart["method"], multipart["partSize"])
			}
			uploadID := multipart["uploadId"].(string)
			if _, err := h.pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, h.fixture.workspaceID, h.fixture.sharedUserID); err != nil {
				t.Fatal(err)
			}
			liveS3Data(t, h.graph(t, "blob-upload-part-url.gql", map[string]any{"workspaceId": h.fixture.workspaceID, "key": largeKey, "uploadId": uploadID, "partNumber": 1}, h.fixture.sharedUserID), "workspace")
			first := h.part(t, largeKey, uploadID, 1)
			etag1 := h.putOK(t, first, large[:5<<20])
			resumed := liveS3Data(t, h.graph(t, "blob-upload-create.gql", largeVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			parts := resumed["uploadedParts"].([]any)
			if resumed["uploadId"] != uploadID || len(parts) != 1 || parts[0].(map[string]any)["partNumber"] != float64(1) || strings.Trim(parts[0].(map[string]any)["etag"].(string), "\"") != strings.Trim(etag1, "\"") {
				t.Fatalf("resumed multipart does not match uploaded part: %#v", resumed)
			}
			second := h.part(t, largeKey, uploadID, 2)
			etag2 := h.putOK(t, second, large[5<<20:])
			if retried := h.putOK(t, second, large[5<<20:]); retried != etag2 {
				t.Fatal("retrying an identical part changed its ETag")
			}
			completeLarge := map[string]any{"workspaceId": h.fixture.workspaceID, "key": largeKey, "uploadId": uploadID,
				"parts": []map[string]any{{"partNumber": 1, "etag": "wrong-etag"}, {"partNumber": 2, "etag": etag2}}}
			if result := h.graph(t, "blob-upload-complete.gql", completeLarge, h.fixture.userID); result["errors"] == nil {
				t.Fatal("completion accepted a mismatched ETag")
			}
			var pending string
			if err := h.pool.QueryRow(ctx, `SELECT status FROM blobs WHERE workspace_id=$1 AND key=$2`, h.fixture.workspaceID, largeKey).Scan(&pending); err != nil || pending != "pending" {
				t.Fatalf("rejected completion ledger=%q err=%v", pending, err)
			}
			completeLarge["parts"] = []map[string]any{{"partNumber": 1, "etag": etag1}, {"partNumber": 2, "etag": etag2}}
			var reservation string
			if err := h.pool.QueryRow(ctx, `SELECT reservation_id::text FROM blobs WHERE workspace_id=$1 AND key=$2`, h.fixture.workspaceID, largeKey).Scan(&reservation); err != nil {
				t.Fatal(err)
			}
			tempKey := h.fixture.workspaceID + "/.reservations/" + reservation + "/" + largeKey
			observed, err := h.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(h.bucket), Key: aws.String(tempKey), UploadId: aws.String(uploadID)})
			if err != nil {
				t.Fatal(err)
			}
			if len(observed.Parts) != 2 || aws.ToInt32(observed.Parts[0].PartNumber) != 1 || aws.ToInt32(observed.Parts[1].PartNumber) != 2 {
				t.Fatalf("S3 service did not replace the retried part: listed %d parts, want unique part numbers 1 and 2", len(observed.Parts))
			}
			// Membership revocation must take effect even with an existing session.
			if _, err := h.pool.Exec(ctx, `UPDATE workspace_members SET state='inactive' WHERE workspace_id=$1 AND user_id=$2`, h.fixture.workspaceID, h.fixture.sharedUserID); err != nil {
				t.Fatal(err)
			}
			if result := h.graph(t, "blob-upload-complete.gql", completeLarge, h.fixture.sharedUserID); result["errors"] == nil {
				t.Fatal("unauthorized member completed an upload")
			}
			for range 2 {
				if got := liveS3Data(t, h.graph(t, "blob-upload-complete.gql", completeLarge, h.fixture.userID), "completeBlobUpload"); got != largeKey {
					t.Fatalf("multipart completion key=%v", got)
				}
			}
			h.assertCompleted(t, largeKey, large)
			h.assertLedgerCount(t, 2)
			h.assertObjectCount(t, 2)
			h.assertMultipartCount(t, 0)

			abortedBody := bytes.Repeat([]byte("c"), 10<<20)
			abortedKey := liveS3ContentKey(abortedBody)
			abortedVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": abortedKey, "size": len(abortedBody), "mime": "application/octet-stream"}
			abortInit := liveS3Data(t, h.graph(t, "blob-upload-create.gql", abortedVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			abortID := abortInit["uploadId"].(string)
			h.putOK(t, h.part(t, abortedKey, abortID, 1), abortedBody[:5<<20])
			abortVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": abortedKey, "uploadId": abortID}
			for _, want := range []bool{true, false} {
				if got := liveS3Data(t, h.graph(t, "blob-upload-abort.gql", abortVars, h.fixture.userID), "abortBlobUpload"); got != want {
					t.Fatalf("abort=%v want=%v", got, want)
				}
			}
			h.assertMultipartCount(t, 0)
			replacement := liveS3Data(t, h.graph(t, "blob-upload-create.gql", abortedVars, h.fixture.userID), "createBlobUpload").(map[string]any)
			if replacement["uploadId"] == abortID || len(replacement["uploadedParts"].([]any)) != 0 {
				t.Fatal("replacement reused a cancelled multipart upload")
			}
			h.putOK(t, h.part(t, abortedKey, replacement["uploadId"].(string), 1), abortedBody[:5<<20])
			if _, err := h.pool.Exec(ctx, `UPDATE blobs SET reservation_expires_at=clock_timestamp()-interval '1 second' WHERE workspace_id=$1 AND key=$2`, h.fixture.workspaceID, abortedKey); err != nil {
				t.Fatal(err)
			}
			if cleaned, err := h.service.CleanupExpiredStorageReservationsV1(ctx, 100); err != nil || cleaned != 1 {
				t.Fatalf("expired multipart cleanup=%d err=%v", cleaned, err)
			}
			h.assertMultipartCount(t, 0)
			h.assertLedgerCount(t, 2)

			for _, removeKey := range []string{key, largeKey} {
				deleteVars := map[string]any{"workspaceId": h.fixture.workspaceID, "key": removeKey, "permanently": true}
				if got := liveS3Data(t, h.graph(t, "blob-delete.gql", deleteVars, h.fixture.userID), "deleteBlob"); got != true {
					t.Fatalf("permanent deletion=%v", got)
				}
			}
			h.assertObjectCount(t, 0)
			h.assertMultipartCount(t, 0)
			h.assertLedgerCount(t, 0)
		})
	}
}

type liveS3UploadFixture struct {
	pool    *pgxpool.Pool
	fixture graphqlFixture
	service *blobs.Service
	client  *s3.Client
	bucket  string
	server  *httptest.Server
}

func newLiveS3UploadFixture(t *testing.T, endpoint string, proxy bool) *liveS3UploadFixture {
	t.Helper()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, t.Context(), pool)
	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE blobs (workspace_id text REFERENCES workspaces(id),key text NOT NULL,size integer NOT NULL,mime text NOT NULL,status text NOT NULL DEFAULT 'completed',reservation_id uuid NOT NULL DEFAULT gen_random_uuid(),reservation_expires_at timestamptz,upload_id text,deleted_at timestamptz,created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(workspace_id,key));
		CREATE TABLE comment_attachments (workspace_id text NOT NULL,doc_id text NOT NULL,key text NOT NULL,size integer NOT NULL,mime text NOT NULL,name text NOT NULL,status text NOT NULL DEFAULT 'completed',reservation_id uuid NOT NULL DEFAULT gen_random_uuid(),reservation_expires_at timestamptz,deleted_at timestamptz,created_by text);
		ALTER TABLE updates ADD COLUMN blob bytea, ADD COLUMN IF NOT EXISTS created_at timestamptz;
	`); err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("notes_test_access", "notes_test_secret", ""), HTTPClient: &http.Client{Timeout: 15 * time.Second}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	bucket := "notes-go-test-" + uuid.NewString()
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// This unique bucket was created above; cleanup never targets shared data.
		pending, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Error(err)
			return
		}
		for _, upload := range pending.Uploads {
			if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId}); err != nil {
				t.Error(err)
			}
		}
		objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			t.Error(err)
			return
		}
		for _, object := range objects.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
				t.Error(err)
			}
		}
		if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	baseURL := "http://" + server.Listener.Addr().String()
	uploadConfig := map[string]any{"enabled": true}
	if proxy {
		uploadConfig["urlPrefix"] = baseURL
		uploadConfig["signKey"] = "disposable-upload-test-secret"
	}
	configData, err := json.Marshal(map[string]any{"storages": map[string]any{"blob.storage": map[string]any{
		"provider": "aws-s3", "bucket": bucket, "config": map[string]any{"endpoint": endpoint, "region": "us-east-1", "forcePathStyle": true,
			"credentials": map[string]any{"accessKeyId": "notes_test_access", "secretAccessKey": "notes_test_secret"}, "usePresignedURL": uploadConfig},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	objectStorage, err := storage.NewRuntime(t.Context(), pool, configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	service := blobs.NewService(pool, objectStorage, "cloud", baseURL)
	workspaceAuthorization := workspaces.NewAuthorizationService(workspaces.NewRepositoryWithDeployment(pool, "cloud"))
	manifestHandler := blobmanifesthttp.NewHandler(auth.NewSessionResolver(pool), blobs.NewManifestService(pool, workspaceAuthorization, objectStorage))
	mux := http.NewServeMux()
	mux.Handle("/graphql", NewHandlerWithBlobService(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, workspaces.NewRepositoryWithDeployment(pool, "cloud"), nil, nil, nil, service))
	mux.Handle("/api/storage/", service.ProxyUploadHandler())
	mux.Handle("GET /api/workspaces/{id}/blobs/v1/{name}", manifestHandler)
	server.Config.Handler = mux
	server.Start()
	return &liveS3UploadFixture{pool: pool, fixture: fixture, service: service, client: client, bucket: bucket, server: server}
}

func (h *liveS3UploadFixture) downloadS3Blob(t *testing.T, key string, want []byte) {
	t.Helper()
	query := url.Values{"sourceType": {"currentDoc"}, "docId": {h.fixture.workspaceID}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+"/api/workspaces/"+h.fixture.workspaceID+"/blobs/v1/"+key+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.fixture.sessionID})
	request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: h.fixture.userID})
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		t.Fatalf("source-scoped S3 download status=%d bytes=%d err=%v", response.StatusCode, len(body), err)
	}
	if response.Header.Get("Content-Type") != "text/plain" || response.Header.Get("Content-Length") != strconv.Itoa(len(want)) || response.Header.Get("Cache-Control") != "private, no-store" || response.Header.Get("Last-Modified") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("source-scoped S3 download headers=%v", response.Header)
	}
}

func (h *liveS3UploadFixture) graph(t *testing.T, operation string, variables map[string]any, actor string) map[string]any {
	t.Helper()
	query, ok := liveS3Operations[operation]
	if !ok {
		t.Fatalf("unknown GraphQL operation %q", operation)
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/graphql", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.fixture.sessionID})
		req.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: actor})
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GraphQL HTTP status=%d", response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func liveS3Data(t *testing.T, result map[string]any, field string) any {
	t.Helper()
	if result["errors"] != nil {
		t.Fatalf("GraphQL %s: %#v", field, result["errors"])
	}
	data, ok := result["data"].(map[string]any)
	if !ok || data[field] == nil {
		t.Fatalf("missing GraphQL %s: %#v", field, result)
	}
	return data[field]
}

func (h *liveS3UploadFixture) part(t *testing.T, key, uploadID string, number int) map[string]any {
	t.Helper()
	result := h.graph(t, "blob-upload-part-url.gql", map[string]any{"workspaceId": h.fixture.workspaceID, "key": key, "uploadId": uploadID, "partNumber": number}, h.fixture.userID)
	return liveS3Data(t, result, "workspace").(map[string]any)["blobUploadPartUrl"].(map[string]any)
}

func (h *liveS3UploadFixture) put(t *testing.T, upload map[string]any, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, upload["uploadUrl"].(string), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headers, ok := upload["headers"].(map[string]any); ok {
		for name, value := range headers {
			req.Header.Set(name, value.(string))
		}
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header.Get("ETag")
}

func (h *liveS3UploadFixture) putOK(t *testing.T, upload map[string]any, body []byte) string {
	t.Helper()
	status, etag := h.put(t, upload, body)
	if status != http.StatusOK {
		t.Fatalf("object PUT status=%d", status)
	}
	return etag
}

func (h *liveS3UploadFixture) assertCompleted(t *testing.T, key string, body []byte) {
	t.Helper()
	var status string
	var released bool
	var size int
	if err := h.pool.QueryRow(t.Context(), `SELECT status,reservation_expires_at IS NULL,size FROM blobs WHERE workspace_id=$1 AND key=$2`, h.fixture.workspaceID, key).Scan(&status, &released, &size); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || !released || size != len(body) {
		t.Fatalf("completed ledger=%s/%t/%d", status, released, size)
	}
	object, err := h.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(h.bucket), Key: aws.String(h.fixture.workspaceID + "/" + key)})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	got, err := io.ReadAll(io.LimitReader(object.Body, int64(len(body))+1))
	if err != nil || !bytes.Equal(got, body) || aws.ToInt64(object.ContentLength) != int64(len(body)) {
		t.Fatalf("completed object mismatch: size=%d err=%v", len(got), err)
	}
}

func (h *liveS3UploadFixture) assertLedgerCount(t *testing.T, want int) {
	t.Helper()
	var count int
	if err := h.pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM blobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("blob rows=%d want=%d", count, want)
	}
}

func (h *liveS3UploadFixture) assertObjectCount(t *testing.T, want int) {
	t.Helper()
	objects, err := h.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(h.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects.Contents) != want || aws.ToBool(objects.IsTruncated) {
		t.Fatalf("S3 objects=%d want=%d", len(objects.Contents), want)
	}
}

func (h *liveS3UploadFixture) assertMultipartCount(t *testing.T, want int) {
	t.Helper()
	uploads, err := h.client.ListMultipartUploads(t.Context(), &s3.ListMultipartUploadsInput{Bucket: aws.String(h.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads.Uploads) != want || aws.ToBool(uploads.IsTruncated) {
		t.Fatalf("unfinished S3 multipart uploads=%d want=%d", len(uploads.Uploads), want)
	}
}

func liveS3ContentKey(body []byte) string {
	digest := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
