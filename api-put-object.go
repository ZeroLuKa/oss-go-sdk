/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2015-2017 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ossClient

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/trinet2005/oss-go-sdk/pkg/encrypt"
	"github.com/trinet2005/oss-go-sdk/pkg/s3utils"
	"golang.org/x/net/http/httpguts"
)

// ReplicationStatus represents replication status of object
type ReplicationStatus string

const (
	// ReplicationStatusPending indicates replication is pending
	ReplicationStatusPending ReplicationStatus = "PENDING"
	// ReplicationStatusComplete indicates replication completed ok
	ReplicationStatusComplete ReplicationStatus = "COMPLETED"
	// ReplicationStatusFailed indicates replication failed
	ReplicationStatusFailed ReplicationStatus = "FAILED"
	// ReplicationStatusReplica indicates object is a replica of a source
	ReplicationStatusReplica ReplicationStatus = "REPLICA"
)

// Empty returns true if no replication status set.
func (r ReplicationStatus) Empty() bool {
	return r == ""
}

// AdvancedPutOptions for internal use - to be utilized by replication, ILM transition
// implementation on MinIO server
type AdvancedPutOptions struct {
	SourceVersionID          string
	SourceETag               string
	ReplicationStatus        ReplicationStatus
	SourceMTime              time.Time
	ReplicationRequest       bool
	RetentionTimestamp       time.Time
	TaggingTimestamp         time.Time
	LegalholdTimestamp       time.Time
	ReplicationValidityCheck bool
}

/* trinet: the user can choose which engine's pool to save data to */

type ErasurePoolEngine string

const (
	DefaultEngine ErasurePoolEngine = ""
	HDD           ErasurePoolEngine = "HDD"
	SSD           ErasurePoolEngine = "SSD"
)

/* trinet */

// PutObjectOptions represents options specified by user for PutObject call
type PutObjectOptions struct {
	UserMetadata            map[string]string
	UserTags                map[string]string
	Progress                io.Reader
	ContentType             string
	ContentEncoding         string
	ContentDisposition      string
	ContentLanguage         string
	CacheControl            string
	Mode                    RetentionMode
	RetainUntilDate         time.Time
	ServerSideEncryption    encrypt.ServerSide
	NumThreads              uint
	StorageClass            string
	WebsiteRedirectLocation string
	PartSize                uint64
	LegalHold               LegalHoldStatus
	SendContentMd5          bool
	DisableContentSha256    bool
	DisableMultipart        bool

	// ConcurrentStreamParts will create NumThreads buffers of PartSize bytes,
	// fill them serially and upload them in parallel.
	// This can be used for faster uploads on non-seekable or slow-to-seek input.
	ConcurrentStreamParts bool

	/* trinet */
	MergeMultipart           bool              // merge all parts in CompleteMultipartUpload and trans to a normal object
	PartialUpdateInfo        PartialUpdateInfo // partial update
	AppendMode               bool              // append write, and PartialUpdateInfo parameters conflict
	PreferredEnginePool      ErasurePoolEngine // the user can choose which engine's pool to save data to
	AmzSnowballExtract       bool              // online extract
	MinIOSnowballIgnoreDirs  bool              // ignore dirs when extract upload
	MinIOSnowballUpdateMTime bool              // update mtime when extract upload
	/* trinet */

	Internal AdvancedPutOptions

	customHeaders http.Header
}

/* trinet */
const (
	PartialUpdateInsertMode  = "Insert"
	PartialUpdateReplaceMode = "Replace"
)

type PartialUpdateInfo struct {
	UpdateMode   string
	UpdateOffset string
}

/* trinet */

// SetMatchETag if etag matches while PUT MinIO returns an error
// this is a MinIO specific extension to support optimistic locking
// semantics.
func (opts *PutObjectOptions) SetMatchETag(etag string) {
	if opts.customHeaders == nil {
		opts.customHeaders = http.Header{}
	}
	opts.customHeaders.Set("If-Match", "\""+etag+"\"")
}

// SetMatchETagExcept if etag does not match while PUT MinIO returns an
// error this is a MinIO specific extension to support optimistic locking
// semantics.
func (opts *PutObjectOptions) SetMatchETagExcept(etag string) {
	if opts.customHeaders == nil {
		opts.customHeaders = http.Header{}
	}
	opts.customHeaders.Set("If-None-Match", "\""+etag+"\"")
}

// getNumThreads - gets the number of threads to be used in the multipart
// put object operation
func (opts PutObjectOptions) getNumThreads() (numThreads int) {
	if opts.NumThreads > 0 {
		numThreads = int(opts.NumThreads)
	} else {
		numThreads = totalWorkers
	}
	return
}

// Header - constructs the headers from metadata entered by user in
// PutObjectOptions struct
func (opts PutObjectOptions) Header() (header http.Header) {
	header = make(http.Header)

	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)

	if opts.ContentEncoding != "" {
		header.Set("Content-Encoding", opts.ContentEncoding)
	}
	if opts.ContentDisposition != "" {
		header.Set("Content-Disposition", opts.ContentDisposition)
	}
	if opts.ContentLanguage != "" {
		header.Set("Content-Language", opts.ContentLanguage)
	}
	if opts.CacheControl != "" {
		header.Set("Cache-Control", opts.CacheControl)
	}

	if opts.Mode != "" {
		header.Set(amzLockMode, opts.Mode.String())
	}

	if !opts.RetainUntilDate.IsZero() {
		header.Set("X-Amz-Object-Lock-Retain-Until-Date", opts.RetainUntilDate.Format(time.RFC3339))
	}

	if opts.LegalHold != "" {
		header.Set(amzLegalHoldHeader, opts.LegalHold.String())
	}

	if opts.ServerSideEncryption != nil {
		opts.ServerSideEncryption.Marshal(header)
	}

	if opts.StorageClass != "" {
		header.Set(amzStorageClass, opts.StorageClass)
	}

	if opts.WebsiteRedirectLocation != "" {
		header.Set(amzWebsiteRedirectLocation, opts.WebsiteRedirectLocation)
	}

	if !opts.Internal.ReplicationStatus.Empty() {
		header.Set(amzBucketReplicationStatus, string(opts.Internal.ReplicationStatus))
	}
	if !opts.Internal.SourceMTime.IsZero() {
		header.Set(minIOBucketSourceMTime, opts.Internal.SourceMTime.Format(time.RFC3339Nano))
	}
	if opts.Internal.SourceETag != "" {
		header.Set(minIOBucketSourceETag, opts.Internal.SourceETag)
	}
	if opts.Internal.ReplicationRequest {
		header.Set(minIOBucketReplicationRequest, "true")
	}
	if opts.Internal.ReplicationValidityCheck {
		header.Set(minIOBucketReplicationCheck, "true")
	}
	if !opts.Internal.LegalholdTimestamp.IsZero() {
		header.Set(minIOBucketReplicationObjectLegalHoldTimestamp, opts.Internal.LegalholdTimestamp.Format(time.RFC3339Nano))
	}
	if !opts.Internal.RetentionTimestamp.IsZero() {
		header.Set(minIOBucketReplicationObjectRetentionTimestamp, opts.Internal.RetentionTimestamp.Format(time.RFC3339Nano))
	}
	if !opts.Internal.TaggingTimestamp.IsZero() {
		header.Set(minIOBucketReplicationTaggingTimestamp, opts.Internal.TaggingTimestamp.Format(time.RFC3339Nano))
	}
	/* trinet */
	if opts.PartialUpdateInfo.UpdateMode != "" && opts.PartialUpdateInfo.UpdateOffset != "" {
		header.Set(MinIOPartialUpdateMode, opts.PartialUpdateInfo.UpdateMode)
		header.Set(MinIOPartialUpdateOffset, opts.PartialUpdateInfo.UpdateOffset)
	}
	if opts.AppendMode {
		// TODO: 目前使用局部更新的方式来实现，后续优化成增加part的方式
		header.Set(MinIOPartialUpdateMode, PartialUpdateInsertMode)
		header.Set(MinIOPartialUpdateOffset, "-1")
	}
	if opts.AmzSnowballExtract {
		header.Set(AmzSnowballExtract, "true")
	}
	if opts.MinIOSnowballIgnoreDirs {
		header.Set(MinIOSnowballIgnoreDirs, "true")
	}
	if opts.MergeMultipart {
		header.Set(MinIOMergeMultipart, "true")
	}
	if opts.PreferredEnginePool != "" {
		header.Set(MinIOPoolEngine, string(opts.PreferredEnginePool))
	}
	if opts.MinIOSnowballUpdateMTime {
		header.Set(MinIOSnowballUpdateMTime, "true")
	}
	/* trinet */

	if len(opts.UserTags) != 0 {
		header.Set(amzTaggingHeader, s3utils.TagEncode(opts.UserTags))
	}

	for k, v := range opts.UserMetadata {
		if isAmzHeader(k) || isStandardHeader(k) || isStorageClassHeader(k) {
			header.Set(k, v)
		} else {
			header.Set("x-amz-meta-"+k, v)
		}
	}

	// set any other additional custom headers.
	for k, v := range opts.customHeaders {
		header[k] = v
	}

	return
}

// validate() checks if the UserMetadata map has standard headers or and raises an error if so.
func (opts PutObjectOptions) validate() (err error) {
	/* trinet */
	if opts.AppendMode && opts.PartialUpdateInfo.UpdateMode != "" {
		return errInvalidArgument("AppendMode and PartialUpdateInfo parameters conflict")
	}
	if (opts.AppendMode || opts.PartialUpdateInfo.UpdateMode != "") && !opts.DisableMultipart {
		return errInvalidArgument("AppendMode and PartialUpdateInfo parameters not support in multipart upload mode")
	}
	if opts.PreferredEnginePool != "" && (opts.AppendMode || opts.PartialUpdateInfo.UpdateMode != "") {
		return errInvalidArgument("PreferredEnginePool parameter is only used to transfer new objects")
	}
	/* trinet */

	for k, v := range opts.UserMetadata {
		if !httpguts.ValidHeaderFieldName(k) || isStandardHeader(k) || isSSEHeader(k) || isStorageClassHeader(k) {
			return errInvalidArgument(k + " unsupported user defined metadata name")
		}
		if !httpguts.ValidHeaderFieldValue(v) {
			return errInvalidArgument(v + " unsupported user defined metadata value")
		}
	}
	if opts.Mode != "" && !opts.Mode.IsValid() {
		return errInvalidArgument(opts.Mode.String() + " unsupported retention mode")
	}
	if opts.LegalHold != "" && !opts.LegalHold.IsValid() {
		return errInvalidArgument(opts.LegalHold.String() + " unsupported legal-hold status")
	}
	return nil
}

// completedParts is a collection of parts sortable by their part numbers.
// used for sorting the uploaded parts before completing the multipart request.
type completedParts []CompletePart

func (a completedParts) Len() int           { return len(a) }
func (a completedParts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a completedParts) Less(i, j int) bool { return a[i].PartNumber < a[j].PartNumber }

/* trinet */
func (c *Client) ExtractOnline(ctx context.Context, bucketName string, reader io.Reader, objectSize int64, ignoreDirs bool, UpdateMTime bool,
) (info UploadInfo, err error) {
	if objectSize >= maxPartSize {
		return UploadInfo{}, errors.New("ExtractOnline file is too large")
	}
	if objectSize < 0 {
		return UploadInfo{}, errors.New("ExtractOnline file is too small, extract can't use steaming upload")
	}

	opts := PutObjectOptions{
		AmzSnowballExtract:       true,
		MinIOSnowballIgnoreDirs:  ignoreDirs,
		PartSize:                 maxPartSize,
		DisableMultipart:         true,
		MinIOSnowballUpdateMTime: UpdateMTime,
	}
	objectName := "extractfile"
	return c.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (c *Client) UpdateObject(ctx context.Context, bucketName, objectName string, updateMod string, updateOffset int,
	reader io.Reader, objectSize int64) (UploadInfo, error) {
	if updateMod != PartialUpdateInsertMode && updateMod != PartialUpdateReplaceMode {
		return UploadInfo{}, errors.New("unsupported mode")
	}
	if updateOffset < -1 {
		return UploadInfo{}, errors.New("offset must be greater than -1")
	}
	if objectSize >= maxPartSize {
		return UploadInfo{}, errors.New("update file is too large")
	}
	if objectSize < 0 {
		return UploadInfo{}, errors.New("update file is too small, Update can't use steaming upload")
	}

	updateInfo := PartialUpdateInfo{
		UpdateMode:   updateMod,
		UpdateOffset: strconv.Itoa(updateOffset),
	}
	opts := PutObjectOptions{
		PartialUpdateInfo: updateInfo,
		DisableMultipart:  true,
		PartSize:          maxPartSize,
	}

	return c.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (c *Client) AppendObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64) (UploadInfo, error) {
	if objectSize >= maxPartSize {
		return UploadInfo{}, errors.New("update file is too large")
	}
	if objectSize < 0 {
		return UploadInfo{}, errors.New("update file is too small, Update can't use steaming upload")
	}

	opts := PutObjectOptions{
		AppendMode:       true,
		DisableMultipart: true,
		PartSize:         maxPartSize,
	}

	return c.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

/* trinet */

// PutObject creates an object in a bucket.
//
// You must have WRITE permissions on a bucket to create an object.
//
//   - For size smaller than 16MiB PutObject automatically does a
//     single atomic PUT operation.
//
//   - For size larger than 16MiB PutObject automatically does a
//     multipart upload operation.
//
//   - For size input as -1 PutObject does a multipart Put operation
//     until input stream reaches EOF. Maximum object size that can
//     be uploaded through this operation will be 5TiB.
//
//     WARNING: Passing down '-1' will use memory and these cannot
//     be reused for best outcomes for PutObject(), pass the size always.
//
// NOTE: Upon errors during upload multipart operation is entirely aborted.
func (c *Client) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64,
	opts PutObjectOptions,
) (info UploadInfo, err error) {
	if objectSize < 0 && opts.DisableMultipart {
		return UploadInfo{}, errors.New("object size must be provided with disable multipart upload")
	}

	err = opts.validate()
	if err != nil {
		return UploadInfo{}, err
	}

	return c.putObjectCommon(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (c *Client) putObjectCommon(ctx context.Context, bucketName, objectName string, reader io.Reader, size int64, opts PutObjectOptions) (info UploadInfo, err error) {
	// Check for largest object size allowed.
	if size > int64(maxMultipartPutObjectSize) {
		return UploadInfo{}, errEntityTooLarge(size, maxMultipartPutObjectSize, bucketName, objectName)
	}

	// NOTE: Streaming signature is not supported by GCS.
	if s3utils.IsGoogleEndpoint(*c.endpointURL) {
		return c.putObject(ctx, bucketName, objectName, reader, size, opts)
	}

	partSize := opts.PartSize
	if opts.PartSize == 0 {
		partSize = minPartSize
	}

	if c.overrideSignerType.IsV2() {
		if size >= 0 && size < int64(partSize) || opts.DisableMultipart {
			return c.putObject(ctx, bucketName, objectName, reader, size, opts)
		}
		return c.putObjectMultipart(ctx, bucketName, objectName, reader, size, opts)
	}

	if size < 0 {
		if opts.DisableMultipart {
			return UploadInfo{}, errors.New("no length provided and multipart disabled")
		}
		if opts.ConcurrentStreamParts && opts.NumThreads > 1 {
			return c.putObjectMultipartStreamParallel(ctx, bucketName, objectName, reader, opts)
		}
		return c.putObjectMultipartStreamNoLength(ctx, bucketName, objectName, reader, opts)
	}

	if size < int64(partSize) || opts.DisableMultipart {
		return c.putObject(ctx, bucketName, objectName, reader, size, opts)
	}

	return c.putObjectMultipartStream(ctx, bucketName, objectName, reader, size, opts)
}

func (c *Client) putObjectMultipartStreamNoLength(ctx context.Context, bucketName, objectName string, reader io.Reader, opts PutObjectOptions) (info UploadInfo, err error) {
	// Input validation.
	if err = s3utils.CheckValidBucketName(bucketName); err != nil {
		return UploadInfo{}, err
	}
	if err = s3utils.CheckValidObjectName(objectName); err != nil {
		return UploadInfo{}, err
	}

	// Total data read and written to server. should be equal to
	// 'size' at the end of the call.
	var totalUploadedSize int64

	// Complete multipart upload.
	var complMultipartUpload completeMultipartUpload

	// Calculate the optimal parts info for a given size.
	totalPartsCount, partSize, _, err := OptimalPartInfo(-1, opts.PartSize)
	if err != nil {
		return UploadInfo{}, err
	}

	if !opts.SendContentMd5 {
		if opts.UserMetadata == nil {
			opts.UserMetadata = make(map[string]string, 1)
		}
		opts.UserMetadata["X-Amz-Checksum-Algorithm"] = "CRC32C"
	}

	// Initiate a new multipart upload.
	uploadID, err := c.newUploadID(ctx, bucketName, objectName, opts)
	if err != nil {
		return UploadInfo{}, err
	}
	delete(opts.UserMetadata, "X-Amz-Checksum-Algorithm")

	defer func() {
		if err != nil {
			c.abortMultipartUpload(ctx, bucketName, objectName, uploadID)
		}
	}()

	// Part number always starts with '1'.
	partNumber := 1

	// Initialize parts uploaded map.
	partsInfo := make(map[int]ObjectPart)

	// Create a buffer.
	buf := make([]byte, partSize)

	// Create checksums
	// CRC32C is ~50% faster on AMD64 @ 30GB/s
	var crcBytes []byte
	customHeader := make(http.Header)
	crc := crc32.New(crc32.MakeTable(crc32.Castagnoli))

	for partNumber <= totalPartsCount {
		length, rerr := readFull(reader, buf)
		if rerr == io.EOF && partNumber > 1 {
			break
		}

		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return UploadInfo{}, rerr
		}

		var md5Base64 string
		if opts.SendContentMd5 {
			// Calculate md5sum.
			hash := c.md5Hasher()
			hash.Write(buf[:length])
			md5Base64 = base64.StdEncoding.EncodeToString(hash.Sum(nil))
			hash.Close()
		} else {
			crc.Reset()
			crc.Write(buf[:length])
			cSum := crc.Sum(nil)
			customHeader.Set("x-amz-checksum-crc32c", base64.StdEncoding.EncodeToString(cSum))
			crcBytes = append(crcBytes, cSum...)
		}

		// Update progress reader appropriately to the latest offset
		// as we read from the source.
		rd := newHook(bytes.NewReader(buf[:length]), opts.Progress)

		// Proceed to upload the part.
		p := uploadPartParams{bucketName: bucketName, objectName: objectName, uploadID: uploadID, reader: rd, partNumber: partNumber, md5Base64: md5Base64, size: int64(length), sse: opts.ServerSideEncryption, streamSha256: !opts.DisableContentSha256, customHeader: customHeader}
		objPart, uerr := c.uploadPart(ctx, p)
		if uerr != nil {
			return UploadInfo{}, uerr
		}

		// Save successfully uploaded part metadata.
		partsInfo[partNumber] = objPart

		// Save successfully uploaded size.
		totalUploadedSize += int64(length)

		// Increment part number.
		partNumber++

		// For unknown size, Read EOF we break away.
		// We do not have to upload till totalPartsCount.
		if rerr == io.EOF {
			break
		}
	}

	// Loop over total uploaded parts to save them in
	// Parts array before completing the multipart request.
	for i := 1; i < partNumber; i++ {
		part, ok := partsInfo[i]
		if !ok {
			return UploadInfo{}, errInvalidArgument(fmt.Sprintf("Missing part number %d", i))
		}
		complMultipartUpload.Parts = append(complMultipartUpload.Parts, CompletePart{
			ETag:           part.ETag,
			PartNumber:     part.PartNumber,
			ChecksumCRC32:  part.ChecksumCRC32,
			ChecksumCRC32C: part.ChecksumCRC32C,
			ChecksumSHA1:   part.ChecksumSHA1,
			ChecksumSHA256: part.ChecksumSHA256,
		})
	}

	// Sort all completed parts.
	sort.Sort(completedParts(complMultipartUpload.Parts))

	opts = PutObjectOptions{}
	if len(crcBytes) > 0 {
		// Add hash of hashes.
		crc.Reset()
		crc.Write(crcBytes)
		opts.UserMetadata = map[string]string{"X-Amz-Checksum-Crc32c": base64.StdEncoding.EncodeToString(crc.Sum(nil))}
	}
	uploadInfo, err := c.completeMultipartUpload(ctx, bucketName, objectName, uploadID, complMultipartUpload, opts)
	if err != nil {
		return UploadInfo{}, err
	}

	uploadInfo.Size = totalUploadedSize
	return uploadInfo, nil
}
