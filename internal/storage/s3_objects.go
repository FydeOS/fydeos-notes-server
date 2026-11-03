// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	defaultPresignTTL = time.Minute
	maxPresignTTL     = 7 * 24 * time.Hour
	maxMultipartPages = 10
)

func (b *s3Backend) PutObject(ctx context.Context, key string, body io.Reader, options PutOptions) (ObjectMetadata, error) {
	if err := validateObjectKey(key); err != nil {
		return ObjectMetadata{}, err
	}
	if body == nil {
		return ObjectMetadata{}, errors.New("object body is required")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return ObjectMetadata{}, fmt.Errorf("read object body: %w", err)
	}
	if options.ContentLength != nil && *options.ContentLength != int64(len(data)) {
		return ObjectMetadata{}, errors.New("object content length mismatch")
	}
	contentType := objectContentType(options.ContentType, data)
	checksum := options.ChecksumCRC32
	if checksum == "" {
		checksum = checksumCRC32(data)
	}
	out, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
		ChecksumCRC32: aws.String(checksum),
	})
	if err != nil {
		return ObjectMetadata{}, fmt.Errorf("put S3 object %q: %w", key, err)
	}
	return ObjectMetadata{
		ContentType:   contentType,
		ContentLength: int64(len(data)),
		LastModified:  time.Now().UTC(),
		ChecksumCRC32: checksum,
		ETag:          aws.ToString(out.ETag),
	}, nil
}

func (b *s3Backend) GetObject(ctx context.Context, key string) (io.ReadCloser, ObjectMetadata, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, ObjectMetadata{}, err
	}
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if isObjectNotFound(err) {
		return nil, ObjectMetadata{}, osNotExistError(key)
	}
	if err != nil {
		return nil, ObjectMetadata{}, fmt.Errorf("get S3 object %q: %w", key, err)
	}
	return out.Body, ObjectMetadata{
		ContentType:   defaultContentType(aws.ToString(out.ContentType)),
		ContentLength: aws.ToInt64(out.ContentLength),
		LastModified:  valueTime(out.LastModified),
		ChecksumCRC32: aws.ToString(out.ChecksumCRC32),
		ETag:          aws.ToString(out.ETag),
	}, nil
}

func (b *s3Backend) GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, err
	}
	if err := validateObjectReadRange(offset, length); err != nil {
		return nil, err
	}
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if isObjectNotFound(err) {
		return nil, osNotExistError(key)
	}
	if err != nil {
		return nil, fmt.Errorf("get S3 object range %q: %w", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(io.LimitReader(out.Body, length+1))
	if err != nil {
		return nil, fmt.Errorf("read S3 object range %q: %w", key, err)
	}
	if int64(len(body)) != length {
		return nil, fmt.Errorf("S3 object range %q returned %d bytes, want %d", key, len(body), length)
	}
	return body, nil
}

func (b *s3Backend) HeadObject(ctx context.Context, key string) (*ObjectMetadata, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, err
	}
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if isObjectNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("head S3 object %q: %w", key, err)
	}
	metadata := ObjectMetadata{
		ContentType:   defaultContentType(aws.ToString(out.ContentType)),
		ContentLength: aws.ToInt64(out.ContentLength),
		LastModified:  valueTime(out.LastModified),
		ChecksumCRC32: aws.ToString(out.ChecksumCRC32),
		ETag:          aws.ToString(out.ETag),
	}
	return &metadata, nil
}

func (b *s3Backend) DeleteObject(ctx context.Context, key string) error {
	if err := validateObjectKey(key); err != nil {
		return err
	}
	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("delete S3 object %q: %w", key, err)
	}
	return nil
}

func (b *s3Backend) PresignPut(ctx context.Context, key string, options PutOptions, expires time.Duration) (PresignedRequest, error) {
	if err := validateObjectKey(key); err != nil {
		return PresignedRequest{}, err
	}
	expires, err := normalizedPresignTTL(expires)
	if err != nil {
		return PresignedRequest{}, err
	}
	contentType := defaultContentType(options.ContentType)
	input := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		ContentLength: options.ContentLength,
	}
	if !b.presignSignContentTypeSet || b.presignSignContentTypeForPut {
		input.ContentType = aws.String(contentType)
	}
	request, err := b.presigner.PresignPutObject(ctx, input, func(options *s3.PresignOptions) { options.Expires = expires })
	if err != nil {
		return PresignedRequest{}, fmt.Errorf("presign S3 PUT %q: %w", key, err)
	}
	headers := make(http.Header, 2)
	headers.Set("Content-Type", contentType)
	if options.ContentLength != nil {
		headers.Set("Content-Length", fmt.Sprint(*options.ContentLength))
	}
	return PresignedRequest{URL: request.URL, Headers: headers, ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

func (b *s3Backend) PresignGet(ctx context.Context, key string, expires time.Duration) (PresignedRequest, error) {
	if err := validateObjectKey(key); err != nil {
		return PresignedRequest{}, err
	}
	expires, err := normalizedPresignTTL(expires)
	if err != nil {
		return PresignedRequest{}, err
	}
	request, err := b.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	}, func(options *s3.PresignOptions) { options.Expires = expires })
	if err != nil {
		return PresignedRequest{}, fmt.Errorf("presign S3 GET %q: %w", key, err)
	}
	return PresignedRequest{URL: request.URL, Headers: make(http.Header), ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

func (b *s3Backend) CreateMultipartUpload(ctx context.Context, key string, options PutOptions, expires time.Duration) (MultipartUpload, error) {
	if err := validateObjectKey(key); err != nil {
		return MultipartUpload{}, err
	}
	expires, err := normalizedPresignTTL(expires)
	if err != nil {
		return MultipartUpload{}, err
	}
	input := &s3.CreateMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(key)}
	if options.ContentType != "" {
		input.ContentType = aws.String(options.ContentType)
	}
	out, err := b.client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("create S3 multipart upload %q: %w", key, err)
	}
	if aws.ToString(out.UploadId) == "" {
		return MultipartUpload{}, errors.New("S3 returned an empty multipart upload id")
	}
	return MultipartUpload{UploadID: aws.ToString(out.UploadId), ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

func (b *s3Backend) PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int32, contentLength int64, expires time.Duration) (PresignedRequest, error) {
	if err := validateObjectKey(key); err != nil {
		return PresignedRequest{}, err
	}
	if err := validateMultipartIdentity(uploadID, partNumber); err != nil {
		return PresignedRequest{}, err
	}
	if contentLength <= 0 {
		return PresignedRequest{}, errors.New("multipart part content length must be positive")
	}
	expires, err := normalizedPresignTTL(expires)
	if err != nil {
		return PresignedRequest{}, err
	}
	request, err := b.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(partNumber),
	}, func(options *s3.PresignOptions) { options.Expires = expires })
	if err != nil {
		return PresignedRequest{}, fmt.Errorf("presign S3 multipart part %q: %w", key, err)
	}
	return PresignedRequest{URL: request.URL, Headers: make(http.Header), ExpiresAt: time.Now().UTC().Add(expires)}, nil
}

func (b *s3Backend) UploadPart(ctx context.Context, key, uploadID string, partNumber int32, body io.Reader, contentLength int64) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	if err := validateMultipartIdentity(uploadID, partNumber); err != nil {
		return "", err
	}
	if body == nil || contentLength < 0 {
		return "", errors.New("invalid multipart part body")
	}
	out, err := b.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(partNumber), Body: body, ContentLength: aws.Int64(contentLength),
	})
	if err != nil {
		return "", fmt.Errorf("upload S3 multipart part %d for %q: %w", partNumber, key, err)
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

func (b *s3Backend) ListMultipartUploadParts(ctx context.Context, key, uploadID string) ([]MultipartPart, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, err
	}
	if uploadID == "" {
		return nil, errors.New("multipart upload id is required")
	}
	parts := make([]MultipartPart, 0)
	var marker *string
	for pageNumber := 0; pageNumber < maxMultipartPages; pageNumber++ {
		out, err := b.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
			PartNumberMarker: marker, MaxParts: aws.Int32(1000),
		})
		if err != nil {
			if isObjectNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("list S3 multipart parts for %q: %w", key, err)
		}
		for _, part := range out.Parts {
			parts = append(parts, MultipartPart{
				PartNumber: aws.ToInt32(part.PartNumber), ETag: strings.Trim(aws.ToString(part.ETag), `"`), Size: aws.ToInt64(part.Size),
			})
		}
		if !aws.ToBool(out.IsTruncated) {
			return parts, nil
		}
		if out.NextPartNumberMarker == nil || *out.NextPartNumberMarker == "" || (marker != nil && *marker == *out.NextPartNumberMarker) {
			return nil, errors.New("S3 returned an invalid multipart part marker")
		}
		marker = out.NextPartNumberMarker
	}
	return nil, errors.New("S3 multipart part listing exceeded the page budget")
}

func (b *s3Backend) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error {
	if err := validateObjectKey(key); err != nil {
		return err
	}
	if uploadID == "" || len(parts) == 0 {
		return errors.New("multipart upload id and parts are required")
	}
	ordered := append([]CompletedPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PartNumber < ordered[j].PartNumber })
	completed := make([]types.CompletedPart, len(ordered))
	for i, part := range ordered {
		if part.PartNumber < 1 || part.PartNumber > 10000 || strings.TrimSpace(part.ETag) == "" {
			return errors.New("invalid completed multipart part")
		}
		if i > 0 && ordered[i-1].PartNumber == part.PartNumber {
			return errors.New("duplicate completed multipart part")
		}
		etag := part.ETag
		if !strings.HasPrefix(etag, `"`) {
			etag = `"` + strings.Trim(etag, `"`) + `"`
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(part.PartNumber), ETag: aws.String(etag)}
	}
	_, err := b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return fmt.Errorf("complete S3 multipart upload %q: %w", key, err)
	}
	return nil
}

func (b *s3Backend) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	if err := validateObjectKey(key); err != nil {
		return err
	}
	if uploadID == "" {
		return errors.New("multipart upload id is required")
	}
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil && !isObjectNotFound(err) {
		return fmt.Errorf("abort S3 multipart upload %q: %w", key, err)
	}
	return nil
}

func normalizedPresignTTL(expires time.Duration) (time.Duration, error) {
	if expires <= 0 {
		expires = defaultPresignTTL
	}
	if expires > maxPresignTTL {
		return 0, errors.New("presigned URL expiration exceeds seven days")
	}
	return expires, nil
}

func validateMultipartIdentity(uploadID string, partNumber int32) error {
	if uploadID == "" || partNumber < 1 || partNumber > 10000 {
		return errors.New("invalid multipart upload id or part number")
	}
	return nil
}

func validateObjectKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.ContainsAny(key, `\\%`) {
		return errors.New("invalid object key")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("invalid object key")
		}
		for _, char := range segment {
			if char < 0x20 || char == 0x7f {
				return errors.New("invalid object key")
			}
		}
	}
	return nil
}

func ValidateObjectKey(key string) error {
	return validateObjectKey(key)
}

func validateObjectReadRange(offset, length int64) error {
	if offset < 0 || length <= 0 || length > maxObjectReadRangeBytes || offset > int64(^uint64(0)>>1)-length {
		return errors.New("invalid object read range")
	}
	return nil
}

func objectContentType(contentType string, body []byte) string {
	if contentType != "" {
		return contentType
	}
	if len(body) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(body[:min(len(body), 512)])
}

func defaultContentType(contentType string) string {
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

func checksumCRC32(body []byte) string {
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(body))
	return base64.StdEncoding.EncodeToString(checksum[:])
}

func valueTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func cloneHeader(source map[string][]string) http.Header {
	result := make(http.Header, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func isObjectNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		code := strings.ToLower(apiError.ErrorCode())
		if code == "nosuchkey" || code == "notfound" || code == "nosuchupload" {
			return true
		}
	}
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound
}

func osNotExistError(key string) error {
	return fmt.Errorf("object %q: %w", key, os.ErrNotExist)
}
