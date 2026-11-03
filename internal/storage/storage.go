// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	BlobScope               = "blob"
	AvatarScope             = "avatar"
	CopilotScope            = "copilot"
	deletePageSize          = 1000
	deleteMaxPages          = 100
	maxObjectReadRangeBytes = 256 << 10
)

var (
	ErrReservedWorkspaceID = errors.New("reserved workspace id")
	ErrPageBudgetExceeded  = errors.New("object delete page budget exhausted")
)

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type ScopeConfig struct {
	Provider                string
	Bucket                  string
	Path                    string
	Endpoint                string
	Region                  string
	AccessKeyID             string
	SecretAccessKey         string
	SessionToken            string
	ForcePathStyle          bool
	RequestTimeoutMs        int
	PresignedUploadEnabled  bool
	ProxyUpload             bool
	UploadURLPrefix         string
	UploadSignKey           string
	PresignExpiresInSeconds int
	PresignSignContentType  bool
}

type UploadURLSettings struct {
	URLPrefix   string
	ProxyUpload bool
	PresignTTL  time.Duration
}

type ObjectDeleter interface {
	DeletePrefix(context.Context, string) (int64, error)
}

type ObjectRangeReader interface {
	GetObjectRange(context.Context, string, int64, int64) ([]byte, error)
}

type ObjectMetadata struct {
	ContentType   string
	ContentLength int64
	LastModified  time.Time
	ChecksumCRC32 string
	ETag          string
}

type PutOptions struct {
	ContentType   string
	ContentLength *int64
	ChecksumCRC32 string
}

type PresignedRequest struct {
	URL       string
	Headers   http.Header
	ExpiresAt time.Time
}

type MultipartPart struct {
	PartNumber int32
	ETag       string
	Size       int64
}

type CompletedPart struct {
	PartNumber int32
	ETag       string
}

type MultipartUpload struct {
	UploadID  string
	ExpiresAt time.Time
}

type ObjectStore interface {
	ObjectDeleter
	PutObject(context.Context, string, io.Reader, PutOptions) (ObjectMetadata, error)
	GetObject(context.Context, string) (io.ReadCloser, ObjectMetadata, error)
	HeadObject(context.Context, string) (*ObjectMetadata, error)
	DeleteObject(context.Context, string) error
	PresignPut(context.Context, string, PutOptions, time.Duration) (PresignedRequest, error)
	PresignGet(context.Context, string, time.Duration) (PresignedRequest, error)
	CreateMultipartUpload(context.Context, string, PutOptions, time.Duration) (MultipartUpload, error)
	PresignUploadPart(context.Context, string, string, int32, int64, time.Duration) (PresignedRequest, error)
	UploadPart(context.Context, string, string, int32, io.Reader, int64) (string, error)
	ListMultipartUploadParts(context.Context, string, string) ([]MultipartPart, error)
	CompleteMultipartUpload(context.Context, string, string, []CompletedPart) error
	AbortMultipartUpload(context.Context, string, string) error
}

var ErrOperationUnsupported = errors.New("storage operation is not supported by this provider")

type Runtime struct {
	db                            DB
	backends                      map[string]ObjectDeleter
	objects                       map[string]ObjectStore
	configs                       map[string]ScopeConfig
	workspaceCleanupLeaseDuration time.Duration
}

type providerConfig struct {
	Provider string          `json:"provider"`
	Bucket   string          `json:"bucket"`
	Config   json.RawMessage `json:"config"`
}

type providerSettings struct {
	Path             string                   `json:"path"`
	Endpoint         string                   `json:"endpoint"`
	Region           string                   `json:"region"`
	AccountID        string                   `json:"accountId"`
	Jurisdiction     string                   `json:"jurisdiction"`
	Credentials      *credentialsConfig       `json:"credentials"`
	ForcePathStyle   *bool                    `json:"forcePathStyle"`
	RequestTimeoutMs int                      `json:"requestTimeoutMs"`
	UsePresignedURL  *usePresignedURLSettings `json:"usePresignedURL"`
	Presign          *presignSettings         `json:"presign"`
}

type presignSettings struct {
	ExpiresInSeconds      int   `json:"expiresInSeconds"`
	SignContentTypeForPut *bool `json:"signContentTypeForPut"`
}

type usePresignedURLSettings struct {
	Enabled   bool   `json:"enabled"`
	URLPrefix string `json:"urlPrefix"`
	SignKey   string `json:"signKey"`
}

type credentialsConfig struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
}

type configFile struct {
	Storages map[string]json.RawMessage `json:"storages"`
	Copilot  struct {
		Storage json.RawMessage `json:"storage"`
	} `json:"copilot"`
}

type legacyLookup func(string) (string, bool)

func NewRuntime(ctx context.Context, db DB, configPath string, lookup func(string) (string, bool)) (*Runtime, error) {
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	configs, err := loadScopeConfigs(configPath, lookup)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(ctx, `SELECT id, value FROM app_configs WHERE id IN ('storages.blob.storage', 'storages.avatar.storage', 'copilot.storage')`)
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
			return nil, fmt.Errorf("load object storage overrides: %w", err)
		}
	} else {
		defer rows.Close()
		for rows.Next() {
			var id string
			var raw json.RawMessage
			if err := rows.Scan(&id, &raw); err != nil {
				return nil, fmt.Errorf("read object storage override: %w", err)
			}
			scope := scopeFromConfigID(id)
			if scope == "" {
				continue
			}
			configs[scope], err = decodeProviderConfig(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid %s storage override", scope)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read object storage overrides: %w", err)
		}
	}

	runtime := &Runtime{
		db:       db,
		backends: make(map[string]ObjectDeleter, len(configs)),
		objects:  make(map[string]ObjectStore, len(configs)),
		configs:  configs,
	}
	for scope, config := range configs {
		backend, err := newBackend(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("configure %s object storage: %w", scope, err)
		}
		runtime.backends[scope] = backend
		if objectStore, ok := backend.(ObjectStore); ok {
			runtime.objects[scope] = objectStore
		}
	}
	return runtime, nil
}

func loadScopeConfigs(configPath string, lookup legacyLookup) (map[string]ScopeConfig, error) {
	defaults := map[string]ScopeConfig{
		BlobScope:    {Provider: "fs", Bucket: "blobs", Path: "~/.affine/storage"},
		AvatarScope:  {Provider: "fs", Bucket: "avatars", Path: "~/.affine/storage"},
		CopilotScope: {Provider: "fs", Bucket: "copilot", Path: "~/.affine/storage"},
	}
	legacy, err := legacyStorageConfigs(lookup)
	if err != nil {
		return nil, err
	}
	for scope, config := range legacy {
		defaults[scope] = config
	}
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, errors.New("unable to read runtime config")
		}
		var file configFile
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, errors.New("invalid runtime config")
		}
		for scope, key := range map[string]string{
			BlobScope: "blob.storage", AvatarScope: "avatar.storage", CopilotScope: "copilot.storage",
		} {
			raw := file.Storages[key]
			if scope == CopilotScope && len(file.Copilot.Storage) > 0 {
				raw = file.Copilot.Storage
			}
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			config, err := decodeProviderConfig(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid %s storage config", scope)
			}
			defaults[scope] = config
		}
	}
	return defaults, nil
}

func legacyStorageConfigs(lookup legacyLookup) (map[string]ScopeConfig, error) {
	r2Names := []string{"R2_OBJECT_STORAGE_ACCOUNT_ID", "R2_OBJECT_STORAGE_ACCESS_KEY_ID", "R2_OBJECT_STORAGE_SECRET_ACCESS_KEY"}
	ossNames := []string{"OSS_ACCESS_KEY_ID", "OSS_ACCESS_KEY_SECRET", "OSS_BUCKET", "OSS_REGION"}
	r2Present, r2Values := readLegacyValues(lookup, r2Names)
	ossPresent, ossValues := readLegacyValues(lookup, ossNames)
	if r2Present && ossPresent {
		return nil, errors.New("ambiguous legacy object storage configuration")
	}
	if r2Present {
		for _, key := range r2Names {
			if strings.TrimSpace(r2Values[key]) == "" {
				return nil, errors.New("incomplete legacy R2 object storage configuration")
			}
		}
		account := r2Values[r2Names[0]]
		endpoint := "https://" + account + ".r2.cloudflarestorage.com"
		base := ScopeConfig{
			Provider: "cloudflare-r2", Bucket: "notes", Endpoint: endpoint, Region: "auto",
			AccessKeyID: r2Values[r2Names[1]], SecretAccessKey: r2Values[r2Names[2]], ForcePathStyle: true,
		}
		return map[string]ScopeConfig{BlobScope: base, CopilotScope: base}, nil
	}
	if ossPresent {
		for _, key := range ossNames {
			if strings.TrimSpace(ossValues[key]) == "" {
				return nil, errors.New("incomplete legacy OSS object storage configuration")
			}
		}
		region := ossValues["OSS_REGION"]
		base := ScopeConfig{
			Provider: "aws-s3", Bucket: ossValues["OSS_BUCKET"], Endpoint: "https://oss-" + region + ".aliyuncs.com",
			Region: region, AccessKeyID: ossValues["OSS_ACCESS_KEY_ID"], SecretAccessKey: ossValues["OSS_ACCESS_KEY_SECRET"],
			ForcePathStyle: false,
		}
		return map[string]ScopeConfig{BlobScope: base, CopilotScope: base}, nil
	}
	return nil, nil
}

func readLegacyValues(lookup legacyLookup, keys []string) (bool, map[string]string) {
	present := false
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value, ok := lookup(key)
		if ok {
			present = true
			values[key] = value
		}
	}
	return present, values
}

func scopeFromConfigID(id string) string {
	switch id {
	case "storages.blob.storage":
		return BlobScope
	case "storages.avatar.storage":
		return AvatarScope
	case "copilot.storage":
		return CopilotScope
	default:
		return ""
	}
}

func decodeProviderConfig(raw []byte) (ScopeConfig, error) {
	var value providerConfig
	if err := json.Unmarshal(raw, &value); err != nil {
		return ScopeConfig{}, err
	}
	if value.Bucket == "" || value.Provider == "" {
		return ScopeConfig{}, errors.New("provider and bucket are required")
	}
	var settings providerSettings
	if err := json.Unmarshal(value.Config, &settings); err != nil {
		return ScopeConfig{}, err
	}
	config := ScopeConfig{Provider: value.Provider, Bucket: value.Bucket, Path: settings.Path, Endpoint: settings.Endpoint, Region: settings.Region, RequestTimeoutMs: settings.RequestTimeoutMs}
	if settings.Credentials != nil {
		config.AccessKeyID = settings.Credentials.AccessKeyID
		config.SecretAccessKey = settings.Credentials.SecretAccessKey
		config.SessionToken = settings.Credentials.SessionToken
	}
	if settings.UsePresignedURL != nil && (value.Provider == "aws-s3" || value.Provider == "cloudflare-r2") {
		config.PresignedUploadEnabled = settings.UsePresignedURL.Enabled
		if settings.UsePresignedURL.Enabled {
			config.UploadURLPrefix = strings.TrimSpace(settings.UsePresignedURL.URLPrefix)
			config.UploadSignKey = settings.UsePresignedURL.SignKey
			config.ProxyUpload = config.UploadSignKey != ""
		}
	}
	switch value.Provider {
	case "fs", "assetpack":
		if settings.Path == "" {
			return ScopeConfig{}, errors.New("filesystem path is required")
		}
	case "aws-s3":
		config.PresignExpiresInSeconds = 60
		config.PresignSignContentType = true
		if settings.Region == "" {
			return ScopeConfig{}, errors.New("S3 region is required")
		}
		if settings.ForcePathStyle != nil {
			config.ForcePathStyle = *settings.ForcePathStyle
		}
	case "cloudflare-r2":
		config.PresignExpiresInSeconds = 60
		config.PresignSignContentType = true
		if settings.AccountID == "" {
			return ScopeConfig{}, errors.New("R2 accountId is required")
		}
		if settings.Jurisdiction != "" && settings.Jurisdiction != "default" && settings.Jurisdiction != "eu" {
			return ScopeConfig{}, errors.New("invalid R2 jurisdiction")
		}
		account := settings.AccountID
		if settings.Jurisdiction == "eu" {
			account += ".eu"
		}
		config.Endpoint = "https://" + account + ".r2.cloudflarestorage.com"
		config.Region = settings.Region
		if config.Region == "" {
			config.Region = "auto"
		}
		config.ForcePathStyle = true
	default:
		return ScopeConfig{}, fmt.Errorf("unsupported object storage provider %q", value.Provider)
	}
	if settings.Presign != nil && (value.Provider == "aws-s3" || value.Provider == "cloudflare-r2") {
		if settings.Presign.ExpiresInSeconds < 0 {
			return ScopeConfig{}, errors.New("presign expiration must not be negative")
		}
		if settings.Presign.ExpiresInSeconds > 0 {
			config.PresignExpiresInSeconds = settings.Presign.ExpiresInSeconds
		}
		if settings.Presign.SignContentTypeForPut != nil {
			config.PresignSignContentType = *settings.Presign.SignContentTypeForPut
		}
	}
	if config.RequestTimeoutMs < 0 {
		return ScopeConfig{}, errors.New("request timeout must not be negative")
	}
	return config, nil
}

func newBackend(ctx context.Context, config ScopeConfig) (ObjectDeleter, error) {
	switch config.Provider {
	case "fs", "assetpack":
		root, err := expandHome(config.Path)
		if err != nil {
			return nil, err
		}
		if config.Bucket == "" || filepath.IsAbs(config.Bucket) || filepath.Clean(config.Bucket) != config.Bucket || strings.Contains(config.Bucket, "..") {
			return nil, errors.New("invalid filesystem bucket")
		}
		return &filesystemBackend{root: root, bucket: config.Bucket}, nil
	case "aws-s3", "cloudflare-r2":
		if config.Bucket == "" || config.Region == "" {
			return nil, errors.New("S3 bucket and region are required")
		}
		if (config.AccessKeyID == "") != (config.SecretAccessKey == "") {
			return nil, errors.New("S3 access key and secret must be configured together")
		}
		region := config.Region
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithHTTPClient(&http.Client{Timeout: storageTimeout(config.RequestTimeoutMs)}))
		if err != nil {
			return nil, errors.New("load S3 client configuration")
		}
		if config.AccessKeyID != "" {
			awsCfg.Credentials = credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.SecretAccessKey, config.SessionToken)
		}
		options := []func(*s3.Options){func(options *s3.Options) { options.UsePathStyle = config.ForcePathStyle }}
		if config.Endpoint != "" {
			options = append(options, func(options *s3.Options) { options.BaseEndpoint = aws.String(config.Endpoint) })
		}
		client := s3.NewFromConfig(awsCfg, options...)
		return &s3Backend{
			client: client, presigner: s3.NewPresignClient(client), bucket: config.Bucket,
			presignSignContentTypeForPut: config.PresignSignContentType,
			presignSignContentTypeSet:    true,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported object storage provider %q", config.Provider)
	}
}

func (r *Runtime) Provider(scope string) string {
	return r.configs[scope].Provider
}

func (r *Runtime) PutObject(ctx context.Context, scope, key string, body io.Reader, options PutOptions) (ObjectMetadata, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return ObjectMetadata{}, err
	}
	return backend.PutObject(ctx, key, body, options)
}

func (r *Runtime) GetObject(ctx context.Context, scope, key string) (io.ReadCloser, ObjectMetadata, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	return backend.GetObject(ctx, key)
}

func (r *Runtime) GetObjectRange(ctx context.Context, scope, key string, offset, length int64) ([]byte, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return nil, err
	}
	rangeReader, ok := backend.(ObjectRangeReader)
	if !ok {
		return nil, ErrOperationUnsupported
	}
	return rangeReader.GetObjectRange(ctx, key, offset, length)
}

func (r *Runtime) HeadObject(ctx context.Context, scope, key string) (*ObjectMetadata, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return nil, err
	}
	return backend.HeadObject(ctx, key)
}

func (r *Runtime) DeleteObject(ctx context.Context, scope, key string) error {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return err
	}
	return backend.DeleteObject(ctx, key)
}

func (r *Runtime) PresignPut(ctx context.Context, scope, key string, options PutOptions, expires time.Duration) (PresignedRequest, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return PresignedRequest{}, err
	}
	return backend.PresignPut(ctx, key, options, expires)
}

func (r *Runtime) PresignGet(ctx context.Context, scope, key string, expires time.Duration) (PresignedRequest, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return PresignedRequest{}, err
	}
	return backend.PresignGet(ctx, key, expires)
}

func (r *Runtime) CreateMultipartUpload(ctx context.Context, scope, key string, options PutOptions, expires time.Duration) (MultipartUpload, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return MultipartUpload{}, err
	}
	return backend.CreateMultipartUpload(ctx, key, options, expires)
}

func (r *Runtime) PresignUploadPart(ctx context.Context, scope, key, uploadID string, partNumber int32, contentLength int64, expires time.Duration) (PresignedRequest, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return PresignedRequest{}, err
	}
	return backend.PresignUploadPart(ctx, key, uploadID, partNumber, contentLength, expires)
}

func (r *Runtime) UploadPart(ctx context.Context, scope, key, uploadID string, partNumber int32, body io.Reader, contentLength int64) (string, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return "", err
	}
	return backend.UploadPart(ctx, key, uploadID, partNumber, body, contentLength)
}

func (r *Runtime) ListMultipartUploadParts(ctx context.Context, scope, key, uploadID string) ([]MultipartPart, error) {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return nil, err
	}
	return backend.ListMultipartUploadParts(ctx, key, uploadID)
}

func (r *Runtime) CompleteMultipartUpload(ctx context.Context, scope, key, uploadID string, parts []CompletedPart) error {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return err
	}
	return backend.CompleteMultipartUpload(ctx, key, uploadID, parts)
}

func (r *Runtime) AbortMultipartUpload(ctx context.Context, scope, key, uploadID string) error {
	backend, err := r.objectBackend(scope)
	if err != nil {
		return err
	}
	return backend.AbortMultipartUpload(ctx, key, uploadID)
}

func (r *Runtime) UploadURLSettings(scope string) (UploadURLSettings, bool) {
	config, ok := r.configs[scope]
	if !ok || !config.PresignedUploadEnabled {
		return UploadURLSettings{}, false
	}
	return UploadURLSettings{
		URLPrefix: config.UploadURLPrefix, ProxyUpload: config.ProxyUpload,
		PresignTTL: storagePresignTTL(config.PresignExpiresInSeconds),
	}, true
}

func (r *Runtime) PresignTTL(scope string) time.Duration {
	config := r.configs[scope]
	return storagePresignTTL(config.PresignExpiresInSeconds)
}

func storagePresignTTL(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func (r *Runtime) SignUploadToken(scope, path string, fields []any, expiresAt int64) (string, bool, error) {
	config, ok := r.configs[scope]
	if !ok || !config.ProxyUpload || config.UploadSignKey == "" {
		return "", false, nil
	}
	token, err := signUploadToken(config.UploadSignKey, path, fields, expiresAt)
	return token, true, err
}

func (r *Runtime) VerifyUploadToken(scope, path string, fields []any, expiresAt int64, token string) bool {
	config, ok := r.configs[scope]
	if !ok || !config.ProxyUpload || config.UploadSignKey == "" {
		return false
	}
	return verifyUploadToken(config.UploadSignKey, path, fields, expiresAt, token)
}

func (r *Runtime) objectBackend(scope string) (ObjectStore, error) {
	if backend := r.objects[scope]; backend != nil {
		return backend, nil
	}
	return nil, fmt.Errorf("storage provider is not configured for scope %s", scope)
}

func storageTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return 30 * time.Second
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("unable to locate storage home directory")
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return filepath.Clean(path), nil
}

func (r *Runtime) DeleteWorkspaceObjects(ctx context.Context, workspaceID string, userIDs []string) (int64, error) {
	return r.deleteWorkspaceObjects(ctx, workspaceID, userIDs, true)
}

func (r *Runtime) deleteWorkspaceObjects(ctx context.Context, workspaceID string, userIDs []string, completeCleanupJob bool) (int64, error) {
	if workspaceID == "" || workspaceID == "comment-attachments" {
		return 0, ErrReservedWorkspaceID
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin workspace storage cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockKey := "storage-workspace:" + workspaceID
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	lockTicker := time.NewTicker(25 * time.Millisecond)
	defer lockTicker.Stop()
	locked := false
	for !locked {
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, lockKey).Scan(&locked); err != nil {
			return 0, fmt.Errorf("lock workspace object cleanup: %w", err)
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-deadline.C:
			return 0, errors.New("workspace storage lock timed out")
		case <-lockTicker.C:
		}
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`, workspaceID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("recheck workspace before object deletion: %w", err)
	}
	if exists {
		return 0, nil
	}
	deleted := int64(0)
	for _, target := range []struct{ scope, prefix string }{
		{BlobScope, workspaceID + "/"},
		{BlobScope, "comment-attachments/" + workspaceID + "/"},
		{CopilotScope, "workspace-files/" + workspaceID + "/"},
		{CopilotScope, "context-files/" + workspaceID + "/"},
		{CopilotScope, "artifacts/" + workspaceID + "/"},
	} {
		count, err := r.deletePrefix(ctx, target.scope, target.prefix)
		if err != nil {
			return deleted, err
		}
		deleted += count
	}
	for _, userID := range uniqueStrings(userIDs) {
		if userID == "" || strings.ContainsAny(userID, "/\\") || userID == "." || userID == ".." {
			continue
		}
		count, err := r.deletePrefix(ctx, CopilotScope, userID+"/"+workspaceID+"/")
		if err != nil {
			return deleted, err
		}
		deleted += count
	}
	if completeCleanupJob {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_storage_cleanup_jobs WHERE workspace_id=$1`, workspaceID); err != nil {
			return deleted, fmt.Errorf("complete workspace storage cleanup task: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return deleted, fmt.Errorf("commit workspace object cleanup lock: %w", err)
	}
	return deleted, nil
}

func (r *Runtime) deletePrefix(ctx context.Context, scope, prefix string) (int64, error) {
	backend := r.backends[scope]
	if backend == nil {
		backend = r.backends[BlobScope]
	}
	if backend == nil {
		return 0, fmt.Errorf("storage provider is not configured for scope %s", scope)
	}
	return backend.DeletePrefix(ctx, prefix)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type s3Backend struct {
	client                       *s3.Client
	presigner                    *s3.PresignClient
	bucket                       string
	presignSignContentTypeForPut bool
	presignSignContentTypeSet    bool
}

func (b *s3Backend) DeletePrefix(ctx context.Context, prefix string) (int64, error) {
	if err := validatePrefix(prefix); err != nil {
		return 0, err
	}
	var deleted int64
	for pageNumber := 0; pageNumber < deleteMaxPages; pageNumber++ {
		page, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(b.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(deletePageSize),
		})
		if err != nil {
			return deleted, fmt.Errorf("list S3 objects for prefix %q: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			return deleted, nil
		}
		keys := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, object := range page.Contents {
			if object.Key == nil || !strings.HasPrefix(*object.Key, prefix) {
				continue
			}
			keys = append(keys, types.ObjectIdentifier{Key: object.Key})
		}
		if len(keys) == 0 {
			return deleted, errors.New("S3 returned no matching keys for a nonempty prefix page")
		}
		for start := 0; start < len(keys); start += 1000 {
			end := min(start+1000, len(keys))
			out, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(b.bucket), Delete: &types.Delete{Objects: keys[start:end]},
			})
			if err != nil {
				return deleted, fmt.Errorf("delete S3 objects for prefix %q: %w", prefix, err)
			}
			if len(out.Errors) > 0 {
				failure := out.Errors[0]
				return deleted, fmt.Errorf("delete S3 object %q failed: %s", aws.ToString(failure.Key), aws.ToString(failure.Code))
			}
			deleted += int64(len(keys[start:end]))
		}
	}
	remaining, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return deleted, fmt.Errorf("verify S3 prefix cleanup %q: %w", prefix, err)
	}
	if len(remaining.Contents) != 0 {
		return deleted, ErrPageBudgetExceeded
	}
	return deleted, nil
}

func validatePrefix(prefix string) error {
	normalized := strings.ReplaceAll(prefix, "\\", "/")
	if normalized == "" || strings.HasPrefix(normalized, "/") {
		return errors.New("invalid object prefix")
	}
	for _, segment := range strings.Split(strings.TrimSuffix(normalized, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("invalid object prefix")
		}
	}
	return nil
}
