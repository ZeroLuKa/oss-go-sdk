/*
 * MinIO Go Library for Amazon S3 Compatible Cloud Storage
 * Copyright 2015-2020 MinIO, Inc.
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
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/trinet2005/oss-go-sdk/pkg/s3utils"
)

/* trinet */
type SimpleBucketInfo struct {
	Name             string    `json:"name"`
	Size             uint64    `json:"size"`
	ObjectsCount     uint64    `json:"objectsCount"`
	CreationDate     time.Time `json:"creationDate"`
	VersioningStatus string    `json:"versioningStatus"`
	RecycleEnabled   bool      `json:"recycleEnabled"`
}

/* trinet */

// ListBuckets list all buckets owned by this authenticated user.
//
// This call requires explicit authentication, no anonymous requests are
// allowed for listing buckets.
//
//	api := client.New(....)
//	for message := range api.ListBuckets(context.Background()) {
//	    fmt.Println(message)
//	}
/* trinet */
func (c *Client) TriListBuckets(ctx context.Context, listRecycle bool) ([]SimpleBucketInfo, error) {
	// Execute GET on service.
	var customHeader http.Header
	if listRecycle {
		headers := make(http.Header)
		headers.Add("X-Minio-List-Recycle-Bucket", "true")
		customHeader = headers
	}
	if customHeader == nil {
		customHeader = make(http.Header)
	}
	customHeader.Add("Accept-Encoding", "gzip")

	urlValues := make(url.Values)
	urlValues.Set("trilistbuckets", "true")

	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		contentSHA256Hex: emptySHA256Hex,
		customHeader:     customHeader,
		queryValues:      urlValues,
	})

	defer closeResponse(resp)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, httpRespToErrorResponse(resp, "", "")
		}
	}

	// 检查响应是否是 gzip 压缩的
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建 gzip reader 失败: %v", err)
		}
		defer gzReader.Close()
		reader = gzReader

		var bucketsInfo []SimpleBucketInfo
		if err = json.NewDecoder(reader).Decode(&bucketsInfo); err != nil {
			return nil, err
		}
		return bucketsInfo, nil
	}

	var bucketsInfo []SimpleBucketInfo
	if err = json.NewDecoder(resp.Body).Decode(&bucketsInfo); err != nil {
		return nil, err
	}
	return bucketsInfo, nil
}

// ListBuckets list all buckets owned by this authenticated user.
//
// This call requires explicit authentication, no anonymous requests are
// allowed for listing buckets.
//
//	api := client.New(....)
//	for message := range api.ListBuckets(context.Background()) {
//	    fmt.Println(message)
//	}
func (c *Client) ListBuckets(ctx context.Context, listRecycle bool) ([]BucketInfo, error) {
	// Execute GET on service.
	/* trinet */
	var customHeader http.Header
	if listRecycle {
		headers := make(http.Header)
		headers.Add("X-Minio-List-Recycle-Bucket", "true")
		customHeader = headers
	}

	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		contentSHA256Hex: emptySHA256Hex,
		customHeader:     customHeader,
	})
	/* trinet */

	defer closeResponse(resp)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, httpRespToErrorResponse(resp, "", "")
		}
	}
	listAllMyBucketsResult := listAllMyBucketsResult{}
	err = xmlDecoder(resp.Body, &listAllMyBucketsResult)
	if err != nil {
		return nil, err
	}
	return listAllMyBucketsResult.Buckets.Bucket, nil
}

// Bucket List Operations.
func (c *Client) listObjectsV2(ctx context.Context, bucketName string, opts ListObjectsOptions) <-chan ObjectInfo {
	// Allocate new list objects channel.
	objectStatCh := make(chan ObjectInfo, 1)
	// Default listing is delimited at "/"
	delimiter := "/"
	if opts.Recursive {
		// If recursive we do not delimit.
		delimiter = ""
	}

	// Return object owner information by default
	fetchOwner := true

	sendObjectInfo := func(info ObjectInfo) {
		select {
		case objectStatCh <- info:
		case <-ctx.Done():
		}
	}

	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		defer close(objectStatCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return objectStatCh
	}

	// Validate incoming object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(opts.Prefix); err != nil {
		defer close(objectStatCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return objectStatCh
	}

	// Initiate list objects goroutine here.
	go func(objectStatCh chan<- ObjectInfo) {
		defer func() {
			if contextCanceled(ctx) {
				objectStatCh <- ObjectInfo{
					Err: ctx.Err(),
				}
			}
			close(objectStatCh)
		}()

		// Save continuationToken for next request.
		var continuationToken string
		for {
			// Get list of objects a maximum of 1000 per request.
			result, err := c.listObjectsV2Query(ctx, bucketName, opts.Prefix, continuationToken,
				fetchOwner, opts.WithMetadata, delimiter, opts.StartAfter, opts.MaxKeys, opts.headers)
			if err != nil {
				sendObjectInfo(ObjectInfo{
					Err: err,
				})
				return
			}

			// If contents are available loop through and send over channel.
			for _, object := range result.Contents {
				object.ETag = trimEtag(object.ETag)
				select {
				// Send object content.
				case objectStatCh <- object:
				// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// Send all common prefixes if any.
			// NOTE: prefixes are only present if the request is delimited.
			for _, obj := range result.CommonPrefixes {
				select {
				// Send object prefixes.
				case objectStatCh <- ObjectInfo{Key: obj.Prefix}:
				// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// If continuation token present, save it for next request.
			if result.NextContinuationToken != "" {
				continuationToken = result.NextContinuationToken
			}

			// Listing ends result is not truncated, return right here.
			if !result.IsTruncated {
				return
			}

			// Add this to catch broken S3 API implementations.
			if continuationToken == "" {
				sendObjectInfo(ObjectInfo{
					Err: fmt.Errorf("listObjectsV2 is truncated without continuationToken, %s S3 server is incompatible with S3 API", c.endpointURL),
				})
				return
			}
		}
	}(objectStatCh)
	return objectStatCh
}

// listObjectsV2Query - (List Objects V2) - List some or all (up to 1000) of the objects in a bucket.
//
// You can use the request parameters as selection criteria to return a subset of the objects in a bucket.
// request parameters :-
// ---------
// ?prefix - Limits the response to keys that begin with the specified prefix.
// ?continuation-token - Used to continue iterating over a set of objects
// ?metadata - Specifies if we want metadata for the objects as part of list operation.
// ?delimiter - A delimiter is a character you use to group keys.
// ?start-after - Sets a marker to start listing lexically at this key onwards.
// ?max-keys - Sets the maximum number of keys returned in the response body.
func (c *Client) listObjectsV2Query(ctx context.Context, bucketName, objectPrefix, continuationToken string, fetchOwner, metadata bool, delimiter, startAfter string, maxkeys int, headers http.Header) (ListBucketV2Result, error) {
	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return ListBucketV2Result{}, err
	}
	// Validate object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(objectPrefix); err != nil {
		return ListBucketV2Result{}, err
	}
	// Get resources properly escaped and lined up before
	// using them in http request.
	urlValues := make(url.Values)

	// Always set list-type in ListObjects V2
	urlValues.Set("list-type", "2")

	if metadata {
		urlValues.Set("metadata", "true")
	}

	// Set this conditionally if asked
	if startAfter != "" {
		urlValues.Set("start-after", startAfter)
	}

	// Always set encoding-type in ListObjects V2
	urlValues.Set("encoding-type", "url")

	// Set object prefix, prefix value to be set to empty is okay.
	urlValues.Set("prefix", objectPrefix)

	// Set delimiter, delimiter value to be set to empty is okay.
	urlValues.Set("delimiter", delimiter)

	// Set continuation token
	if continuationToken != "" {
		urlValues.Set("continuation-token", continuationToken)
	}

	// Fetch owner when listing
	if fetchOwner {
		urlValues.Set("fetch-owner", "true")
	}

	// Set max keys.
	if maxkeys > 0 {
		urlValues.Set("max-keys", fmt.Sprintf("%d", maxkeys))
	}

	// Execute GET on bucket to list objects.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		queryValues:      urlValues,
		contentSHA256Hex: emptySHA256Hex,
		customHeader:     headers,
	})
	defer closeResponse(resp)
	if err != nil {
		return ListBucketV2Result{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ListBucketV2Result{}, httpRespToErrorResponse(resp, bucketName, "")
		}
	}

	// Decode listBuckets XML.
	listBucketResult := ListBucketV2Result{}
	if err = xmlDecoder(resp.Body, &listBucketResult); err != nil {
		return listBucketResult, err
	}

	// This is an additional verification check to make
	// sure proper responses are received.
	if listBucketResult.IsTruncated && listBucketResult.NextContinuationToken == "" {
		return listBucketResult, ErrorResponse{
			Code:    "NotImplemented",
			Message: "Truncated response should have continuation token set",
		}
	}

	for i, obj := range listBucketResult.Contents {
		listBucketResult.Contents[i].Key, err = decodeS3Name(obj.Key, listBucketResult.EncodingType)
		if err != nil {
			return listBucketResult, err
		}
		listBucketResult.Contents[i].LastModified = listBucketResult.Contents[i].LastModified.Truncate(time.Millisecond)
	}

	for i, obj := range listBucketResult.CommonPrefixes {
		listBucketResult.CommonPrefixes[i].Prefix, err = decodeS3Name(obj.Prefix, listBucketResult.EncodingType)
		if err != nil {
			return listBucketResult, err
		}
	}

	// Success.
	return listBucketResult, nil
}

func (c *Client) listObjects(ctx context.Context, bucketName string, opts ListObjectsOptions) <-chan ObjectInfo {
	// Allocate new list objects channel.
	objectStatCh := make(chan ObjectInfo, 1)
	// Default listing is delimited at "/"
	delimiter := "/"
	if opts.Recursive {
		// If recursive we do not delimit.
		delimiter = ""
	}

	sendObjectInfo := func(info ObjectInfo) {
		select {
		case objectStatCh <- info:
		case <-ctx.Done():
		}
	}

	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		defer close(objectStatCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return objectStatCh
	}
	// Validate incoming object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(opts.Prefix); err != nil {
		defer close(objectStatCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return objectStatCh
	}

	// Initiate list objects goroutine here.
	go func(objectStatCh chan<- ObjectInfo) {
		defer func() {
			if contextCanceled(ctx) {
				objectStatCh <- ObjectInfo{
					Err: ctx.Err(),
				}
			}
			close(objectStatCh)
		}()

		marker := opts.StartAfter
		for {
			// Get list of objects a maximum of 1000 per request.
			result, err := c.listObjectsQuery(ctx, bucketName, opts.Prefix, marker, delimiter, opts.MaxKeys, opts.headers)
			if err != nil {
				sendObjectInfo(ObjectInfo{
					Err: err,
				})
				return
			}

			// If contents are available loop through and send over channel.
			for _, object := range result.Contents {
				// Save the marker.
				marker = object.Key
				object.ETag = trimEtag(object.ETag)
				select {
				// Send object content.
				case objectStatCh <- object:
				// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// Send all common prefixes if any.
			// NOTE: prefixes are only present if the request is delimited.
			for _, obj := range result.CommonPrefixes {
				select {
				// Send object prefixes.
				case objectStatCh <- ObjectInfo{Key: obj.Prefix}:
				// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// If next marker present, save it for next request.
			if result.NextMarker != "" {
				marker = result.NextMarker
			}

			// Listing ends result is not truncated, return right here.
			if !result.IsTruncated {
				return
			}
		}
	}(objectStatCh)
	return objectStatCh
}

func (c *Client) listObjectVersions(ctx context.Context, bucketName string, opts ListObjectsOptions) <-chan ObjectInfo {
	// Allocate new list objects channel.
	resultCh := make(chan ObjectInfo, 1)
	// Default listing is delimited at "/"
	delimiter := "/"
	if opts.Recursive {
		// If recursive we do not delimit.
		delimiter = ""
	}

	sendObjectInfo := func(info ObjectInfo) {
		select {
		case resultCh <- info:
		case <-ctx.Done():
		}
	}

	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		defer close(resultCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return resultCh
	}

	// Validate incoming object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(opts.Prefix); err != nil {
		defer close(resultCh)
		sendObjectInfo(ObjectInfo{
			Err: err,
		})
		return resultCh
	}

	// Initiate list objects goroutine here.
	go func(resultCh chan<- ObjectInfo) {
		defer func() {
			if contextCanceled(ctx) {
				resultCh <- ObjectInfo{
					Err: ctx.Err(),
				}
			}
			close(resultCh)
		}()

		var (
			keyMarker       = ""
			versionIDMarker = ""
		)

		for {
			// Get list of objects a maximum of 1000 per request.
			result, err := c.listObjectVersionsQuery(ctx, bucketName, opts, keyMarker, versionIDMarker, delimiter)
			if err != nil {
				sendObjectInfo(ObjectInfo{
					Err: err,
				})
				return
			}

			// If contents are available loop through and send over channel.
			for _, version := range result.Versions {
				info := ObjectInfo{
					ETag:           trimEtag(version.ETag),
					Key:            version.Key,
					LastModified:   version.LastModified.Truncate(time.Millisecond),
					Size:           version.Size,
					Owner:          version.Owner,
					StorageClass:   version.StorageClass,
					IsLatest:       version.IsLatest,
					VersionID:      version.VersionID,
					IsDeleteMarker: version.isDeleteMarker,
					UserTags:       version.UserTags,
					UserMetadata:   version.UserMetadata,
					Internal:       version.Internal,
				}
				select {
				// Send object version info.
				case resultCh <- info:
					// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// Send all common prefixes if any.
			// NOTE: prefixes are only present if the request is delimited.
			for _, obj := range result.CommonPrefixes {
				select {
				// Send object prefixes.
				case resultCh <- ObjectInfo{Key: obj.Prefix}:
				// If receives done from the caller, return here.
				case <-ctx.Done():
					return
				}
			}

			// If next key marker is present, save it for next request.
			if result.NextKeyMarker != "" {
				keyMarker = result.NextKeyMarker
			}

			// If next version id marker is present, save it for next request.
			if result.NextVersionIDMarker != "" {
				versionIDMarker = result.NextVersionIDMarker
			}

			// Listing ends result is not truncated, return right here.
			if !result.IsTruncated {
				return
			}
		}
	}(resultCh)
	return resultCh
}

// listObjectVersions - (List Object Versions) - List some or all (up to 1000) of the existing objects
// and their versions in a bucket.
//
// You can use the request parameters as selection criteria to return a subset of the objects in a bucket.
// request parameters :-
// ---------
// ?key-marker - Specifies the key to start with when listing objects in a bucket.
// ?version-id-marker - Specifies the version id marker to start with when listing objects with versions in a bucket.
// ?delimiter - A delimiter is a character you use to group keys.
// ?prefix - Limits the response to keys that begin with the specified prefix.
// ?max-keys - Sets the maximum number of keys returned in the response body.
func (c *Client) listObjectVersionsQuery(ctx context.Context, bucketName string, opts ListObjectsOptions, keyMarker, versionIDMarker, delimiter string) (ListVersionsResult, error) {
	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return ListVersionsResult{}, err
	}
	// Validate object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(opts.Prefix); err != nil {
		return ListVersionsResult{}, err
	}
	// Get resources properly escaped and lined up before
	// using them in http request.
	urlValues := make(url.Values)

	// Set versions to trigger versioning API
	urlValues.Set("versions", "")

	// Set object prefix, prefix value to be set to empty is okay.
	urlValues.Set("prefix", opts.Prefix)

	// Set delimiter, delimiter value to be set to empty is okay.
	urlValues.Set("delimiter", delimiter)

	// Set object marker.
	if keyMarker != "" {
		urlValues.Set("key-marker", keyMarker)
	}

	// Set max keys.
	if opts.MaxKeys > 0 {
		urlValues.Set("max-keys", fmt.Sprintf("%d", opts.MaxKeys))
	}

	// Set version ID marker
	if versionIDMarker != "" {
		urlValues.Set("version-id-marker", versionIDMarker)
	}

	if opts.WithMetadata {
		urlValues.Set("metadata", "true")
	}

	// Always set encoding-type
	urlValues.Set("encoding-type", "url")

	// Execute GET on bucket to list objects.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		queryValues:      urlValues,
		contentSHA256Hex: emptySHA256Hex,
		customHeader:     opts.headers,
	})
	defer closeResponse(resp)
	if err != nil {
		return ListVersionsResult{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ListVersionsResult{}, httpRespToErrorResponse(resp, bucketName, "")
		}
	}

	// Decode ListVersionsResult XML.
	listObjectVersionsOutput := ListVersionsResult{}
	err = xmlDecoder(resp.Body, &listObjectVersionsOutput)
	if err != nil {
		return ListVersionsResult{}, err
	}

	for i, obj := range listObjectVersionsOutput.Versions {
		listObjectVersionsOutput.Versions[i].Key, err = decodeS3Name(obj.Key, listObjectVersionsOutput.EncodingType)
		if err != nil {
			return listObjectVersionsOutput, err
		}
	}

	for i, obj := range listObjectVersionsOutput.CommonPrefixes {
		listObjectVersionsOutput.CommonPrefixes[i].Prefix, err = decodeS3Name(obj.Prefix, listObjectVersionsOutput.EncodingType)
		if err != nil {
			return listObjectVersionsOutput, err
		}
	}

	if listObjectVersionsOutput.NextKeyMarker != "" {
		listObjectVersionsOutput.NextKeyMarker, err = decodeS3Name(listObjectVersionsOutput.NextKeyMarker, listObjectVersionsOutput.EncodingType)
		if err != nil {
			return listObjectVersionsOutput, err
		}
	}

	return listObjectVersionsOutput, nil
}

// listObjects - (List Objects) - List some or all (up to 1000) of the objects in a bucket.
//
// You can use the request parameters as selection criteria to return a subset of the objects in a bucket.
// request parameters :-
// ---------
// ?marker - Specifies the key to start with when listing objects in a bucket.
// ?delimiter - A delimiter is a character you use to group keys.
// ?prefix - Limits the response to keys that begin with the specified prefix.
// ?max-keys - Sets the maximum number of keys returned in the response body.
func (c *Client) listObjectsQuery(ctx context.Context, bucketName, objectPrefix, objectMarker, delimiter string, maxkeys int, headers http.Header) (ListBucketResult, error) {
	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return ListBucketResult{}, err
	}
	// Validate object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(objectPrefix); err != nil {
		return ListBucketResult{}, err
	}
	// Get resources properly escaped and lined up before
	// using them in http request.
	urlValues := make(url.Values)

	// Set object prefix, prefix value to be set to empty is okay.
	urlValues.Set("prefix", objectPrefix)

	// Set delimiter, delimiter value to be set to empty is okay.
	urlValues.Set("delimiter", delimiter)

	// Set object marker.
	if objectMarker != "" {
		urlValues.Set("marker", objectMarker)
	}

	// Set max keys.
	if maxkeys > 0 {
		urlValues.Set("max-keys", fmt.Sprintf("%d", maxkeys))
	}

	// Always set encoding-type
	urlValues.Set("encoding-type", "url")

	// Execute GET on bucket to list objects.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		queryValues:      urlValues,
		contentSHA256Hex: emptySHA256Hex,
		customHeader:     headers,
	})
	defer closeResponse(resp)
	if err != nil {
		return ListBucketResult{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ListBucketResult{}, httpRespToErrorResponse(resp, bucketName, "")
		}
	}
	// Decode listBuckets XML.
	listBucketResult := ListBucketResult{}
	err = xmlDecoder(resp.Body, &listBucketResult)
	if err != nil {
		return listBucketResult, err
	}

	for i, obj := range listBucketResult.Contents {
		listBucketResult.Contents[i].Key, err = decodeS3Name(obj.Key, listBucketResult.EncodingType)
		if err != nil {
			return listBucketResult, err
		}
		listBucketResult.Contents[i].LastModified = listBucketResult.Contents[i].LastModified.Truncate(time.Millisecond)
	}

	for i, obj := range listBucketResult.CommonPrefixes {
		listBucketResult.CommonPrefixes[i].Prefix, err = decodeS3Name(obj.Prefix, listBucketResult.EncodingType)
		if err != nil {
			return listBucketResult, err
		}
	}

	if listBucketResult.NextMarker != "" {
		listBucketResult.NextMarker, err = decodeS3Name(listBucketResult.NextMarker, listBucketResult.EncodingType)
		if err != nil {
			return listBucketResult, err
		}
	}

	return listBucketResult, nil
}

// ListObjectsOptions holds all options of a list object request
type ListObjectsOptions struct {
	// Include objects versions in the listing
	WithVersions bool
	// Include objects metadata in the listing
	WithMetadata bool
	// Only list objects with the prefix
	Prefix string
	// Ignore '/' delimiter
	Recursive bool
	// The maximum number of objects requested per
	// batch, advanced use-case not useful for most
	// applications
	MaxKeys int
	// StartAfter start listing lexically at this
	// object onwards, this value can also be set
	// for Marker when `UseV1` is set to true.
	StartAfter string

	// Use the deprecated list objects V1 API
	UseV1 bool

	headers http.Header
}

// Set adds a key value pair to the options. The
// key-value pair will be part of the HTTP GET request
// headers.
func (o *ListObjectsOptions) Set(key, value string) {
	if o.headers == nil {
		o.headers = make(http.Header)
	}
	o.headers.Set(key, value)
}

/* trinet */
type getBucketDetailInfo struct {
	XMLName      xml.Name `xml:"GetBucketDetailInfo"`
	CreationDate string   `xml:"CreationDate"`
	Size         string   `xml:"Size"`
	ObjNum       string   `xml:"ObjNum"`
}

// GetBucketDetailInfo get the bucket creat time size and object count
func (c *Client) GetBucketDetailInfo(ctx context.Context, bucketName string) (string, string, string, error) {
	// Input validation.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		return "", "", "", err
	}

	urlValues := make(url.Values)
	urlValues.Set("getBucketDetailInfo", "")

	// Execute PUT on bucket.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		contentSHA256Hex: emptySHA256Hex,
		queryValues:      urlValues,
	})
	defer closeResponse(resp)
	if err != nil {
		return "", "", "", err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return "", "", "", httpRespToErrorResponse(resp, bucketName, "")
		}
	}
	info := &getBucketDetailInfo{}
	if err = xml.NewDecoder(resp.Body).Decode(info); err != nil {
		return "", "", "", err
	}

	return info.CreationDate, info.Size, info.ObjNum, nil
}

/* trinet */

// ListObjects returns objects list after evaluating the passed options.
//
//	api := client.New(....)
//	for object := range api.ListObjects(ctx, "mytestbucket", minio.ListObjectsOptions{Prefix: "starthere", Recursive:true}) {
//	    fmt.Println(object)
//	}
//
// If caller cancels the context, then the last entry on the 'chan ObjectInfo' will be the context.Error()
// caller must drain the channel entirely and wait until channel is closed before proceeding, without
// waiting on the channel to be closed completely you might leak goroutines.
func (c *Client) ListObjects(ctx context.Context, bucketName string, opts ListObjectsOptions) <-chan ObjectInfo {
	if opts.WithVersions {
		return c.listObjectVersions(ctx, bucketName, opts)
	}

	// Use legacy list objects v1 API
	if opts.UseV1 {
		return c.listObjects(ctx, bucketName, opts)
	}

	// Check whether this is snowball region, if yes ListObjectsV2 doesn't work, fallback to listObjectsV1.
	if location, ok := c.bucketLocCache.Get(bucketName); ok {
		if location == "snowball" {
			return c.listObjects(ctx, bucketName, opts)
		}
	}

	return c.listObjectsV2(ctx, bucketName, opts)
}

// ListIncompleteUploads - List incompletely uploaded multipart objects.
//
// ListIncompleteUploads lists all incompleted objects matching the
// objectPrefix from the specified bucket. If recursion is enabled
// it would list all subdirectories and all its contents.
//
// Your input parameters are just bucketName, objectPrefix, recursive.
// If you enable recursive as 'true' this function will return back all
// the multipart objects in a given bucket name.
//
//	api := client.New(....)
//	// Recurively list all objects in 'mytestbucket'
//	recursive := true
//	for message := range api.ListIncompleteUploads(context.Background(), "mytestbucket", "starthere", recursive) {
//	    fmt.Println(message)
//	}
func (c *Client) ListIncompleteUploads(ctx context.Context, bucketName, objectPrefix string, recursive bool) <-chan ObjectMultipartInfo {
	return c.listIncompleteUploads(ctx, bucketName, objectPrefix, recursive)
}

// contextCanceled returns whether a context is canceled.
func contextCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// listIncompleteUploads lists all incomplete uploads.
func (c *Client) listIncompleteUploads(ctx context.Context, bucketName, objectPrefix string, recursive bool) <-chan ObjectMultipartInfo {
	// Allocate channel for multipart uploads.
	objectMultipartStatCh := make(chan ObjectMultipartInfo, 1)
	// Delimiter is set to "/" by default.
	delimiter := "/"
	if recursive {
		// If recursive do not delimit.
		delimiter = ""
	}
	// Validate bucket name.
	if err := s3utils.CheckValidBucketName(bucketName); err != nil {
		defer close(objectMultipartStatCh)
		objectMultipartStatCh <- ObjectMultipartInfo{
			Err: err,
		}
		return objectMultipartStatCh
	}
	// Validate incoming object prefix.
	if err := s3utils.CheckValidObjectNamePrefix(objectPrefix); err != nil {
		defer close(objectMultipartStatCh)
		objectMultipartStatCh <- ObjectMultipartInfo{
			Err: err,
		}
		return objectMultipartStatCh
	}
	go func(objectMultipartStatCh chan<- ObjectMultipartInfo) {
		defer func() {
			if contextCanceled(ctx) {
				objectMultipartStatCh <- ObjectMultipartInfo{
					Err: ctx.Err(),
				}
			}
			close(objectMultipartStatCh)
		}()

		// object and upload ID marker for future requests.
		var objectMarker string
		var uploadIDMarker string
		for {
			// list all multipart uploads.
			result, err := c.listMultipartUploadsQuery(ctx, bucketName, objectMarker, uploadIDMarker, objectPrefix, delimiter, 0)
			if err != nil {
				objectMultipartStatCh <- ObjectMultipartInfo{
					Err: err,
				}
				return
			}
			objectMarker = result.NextKeyMarker
			uploadIDMarker = result.NextUploadIDMarker

			// Send all multipart uploads.
			for _, obj := range result.Uploads {
				// Calculate total size of the uploaded parts if 'aggregateSize' is enabled.
				select {
				// Send individual uploads here.
				case objectMultipartStatCh <- obj:
				// If the context is canceled
				case <-ctx.Done():
					return
				}
			}
			// Send all common prefixes if any.
			// NOTE: prefixes are only present if the request is delimited.
			for _, obj := range result.CommonPrefixes {
				select {
				// Send delimited prefixes here.
				case objectMultipartStatCh <- ObjectMultipartInfo{Key: obj.Prefix, Size: 0}:
				// If context is canceled.
				case <-ctx.Done():
					return
				}
			}
			// Listing ends if result not truncated, return right here.
			if !result.IsTruncated {
				return
			}
		}
	}(objectMultipartStatCh)
	// return.
	return objectMultipartStatCh
}

// listMultipartUploadsQuery - (List Multipart Uploads).
//   - Lists some or all (up to 1000) in-progress multipart uploads in a bucket.
//
// You can use the request parameters as selection criteria to return a subset of the uploads in a bucket.
// request parameters. :-
// ---------
// ?key-marker - Specifies the multipart upload after which listing should begin.
// ?upload-id-marker - Together with key-marker specifies the multipart upload after which listing should begin.
// ?delimiter - A delimiter is a character you use to group keys.
// ?prefix - Limits the response to keys that begin with the specified prefix.
// ?max-uploads - Sets the maximum number of multipart uploads returned in the response body.
func (c *Client) listMultipartUploadsQuery(ctx context.Context, bucketName, keyMarker, uploadIDMarker, prefix, delimiter string, maxUploads int) (ListMultipartUploadsResult, error) {
	// Get resources properly escaped and lined up before using them in http request.
	urlValues := make(url.Values)
	// Set uploads.
	urlValues.Set("uploads", "")
	// Set object key marker.
	if keyMarker != "" {
		urlValues.Set("key-marker", keyMarker)
	}
	// Set upload id marker.
	if uploadIDMarker != "" {
		urlValues.Set("upload-id-marker", uploadIDMarker)
	}

	// Set object prefix, prefix value to be set to empty is okay.
	urlValues.Set("prefix", prefix)

	// Set delimiter, delimiter value to be set to empty is okay.
	urlValues.Set("delimiter", delimiter)

	// Always set encoding-type
	urlValues.Set("encoding-type", "url")

	// maxUploads should be 1000 or less.
	if maxUploads > 0 {
		// Set max-uploads.
		urlValues.Set("max-uploads", fmt.Sprintf("%d", maxUploads))
	}

	// Execute GET on bucketName to list multipart uploads.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		queryValues:      urlValues,
		contentSHA256Hex: emptySHA256Hex,
	})
	defer closeResponse(resp)
	if err != nil {
		return ListMultipartUploadsResult{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ListMultipartUploadsResult{}, httpRespToErrorResponse(resp, bucketName, "")
		}
	}
	// Decode response body.
	listMultipartUploadsResult := ListMultipartUploadsResult{}
	err = xmlDecoder(resp.Body, &listMultipartUploadsResult)
	if err != nil {
		return listMultipartUploadsResult, err
	}

	listMultipartUploadsResult.NextKeyMarker, err = decodeS3Name(listMultipartUploadsResult.NextKeyMarker, listMultipartUploadsResult.EncodingType)
	if err != nil {
		return listMultipartUploadsResult, err
	}

	listMultipartUploadsResult.NextUploadIDMarker, err = decodeS3Name(listMultipartUploadsResult.NextUploadIDMarker, listMultipartUploadsResult.EncodingType)
	if err != nil {
		return listMultipartUploadsResult, err
	}

	for i, obj := range listMultipartUploadsResult.Uploads {
		listMultipartUploadsResult.Uploads[i].Key, err = decodeS3Name(obj.Key, listMultipartUploadsResult.EncodingType)
		if err != nil {
			return listMultipartUploadsResult, err
		}
	}

	for i, obj := range listMultipartUploadsResult.CommonPrefixes {
		listMultipartUploadsResult.CommonPrefixes[i].Prefix, err = decodeS3Name(obj.Prefix, listMultipartUploadsResult.EncodingType)
		if err != nil {
			return listMultipartUploadsResult, err
		}
	}

	return listMultipartUploadsResult, nil
}

// listObjectParts list all object parts recursively.
//
//lint:ignore U1000 Keep this around
func (c *Client) listObjectParts(ctx context.Context, bucketName, objectName, uploadID string) (partsInfo map[int]ObjectPart, err error) {
	// Part number marker for the next batch of request.
	var nextPartNumberMarker int
	partsInfo = make(map[int]ObjectPart)
	for {
		// Get list of uploaded parts a maximum of 1000 per request.
		listObjPartsResult, err := c.listObjectPartsQuery(ctx, bucketName, objectName, uploadID, nextPartNumberMarker, 1000)
		if err != nil {
			return nil, err
		}
		// Append to parts info.
		for _, part := range listObjPartsResult.ObjectParts {
			// Trim off the odd double quotes from ETag in the beginning and end.
			part.ETag = trimEtag(part.ETag)
			partsInfo[part.PartNumber] = part
		}
		// Keep part number marker, for the next iteration.
		nextPartNumberMarker = listObjPartsResult.NextPartNumberMarker
		// Listing ends result is not truncated, return right here.
		if !listObjPartsResult.IsTruncated {
			break
		}
	}

	// Return all the parts.
	return partsInfo, nil
}

// findUploadIDs lists all incomplete uploads and find the uploadIDs of the matching object name.
func (c *Client) findUploadIDs(ctx context.Context, bucketName, objectName string) ([]string, error) {
	var uploadIDs []string
	// Make list incomplete uploads recursive.
	isRecursive := true
	// List all incomplete uploads.
	for mpUpload := range c.listIncompleteUploads(ctx, bucketName, objectName, isRecursive) {
		if mpUpload.Err != nil {
			return nil, mpUpload.Err
		}
		if objectName == mpUpload.Key {
			uploadIDs = append(uploadIDs, mpUpload.UploadID)
		}
	}
	// Return the latest upload id.
	return uploadIDs, nil
}

// listObjectPartsQuery (List Parts query)
//   - lists some or all (up to 1000) parts that have been uploaded
//     for a specific multipart upload
//
// You can use the request parameters as selection criteria to return
// a subset of the uploads in a bucket, request parameters :-
// ---------
// ?part-number-marker - Specifies the part after which listing should
// begin.
// ?max-parts - Maximum parts to be listed per request.
func (c *Client) listObjectPartsQuery(ctx context.Context, bucketName, objectName, uploadID string, partNumberMarker, maxParts int) (ListObjectPartsResult, error) {
	// Get resources properly escaped and lined up before using them in http request.
	urlValues := make(url.Values)
	// Set part number marker.
	urlValues.Set("part-number-marker", fmt.Sprintf("%d", partNumberMarker))
	// Set upload id.
	urlValues.Set("uploadId", uploadID)

	// maxParts should be 1000 or less.
	if maxParts > 0 {
		// Set max parts.
		urlValues.Set("max-parts", fmt.Sprintf("%d", maxParts))
	}

	// Execute GET on objectName to get list of parts.
	resp, err := c.executeMethod(ctx, http.MethodGet, requestMetadata{
		bucketName:       bucketName,
		objectName:       objectName,
		queryValues:      urlValues,
		contentSHA256Hex: emptySHA256Hex,
	})
	defer closeResponse(resp)
	if err != nil {
		return ListObjectPartsResult{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return ListObjectPartsResult{}, httpRespToErrorResponse(resp, bucketName, objectName)
		}
	}
	// Decode list object parts XML.
	listObjectPartsResult := ListObjectPartsResult{}
	err = xmlDecoder(resp.Body, &listObjectPartsResult)
	if err != nil {
		return listObjectPartsResult, err
	}
	return listObjectPartsResult, nil
}

// Decode an S3 object name according to the encoding type
func decodeS3Name(name, encodingType string) (string, error) {
	switch encodingType {
	case "url":
		return url.QueryUnescape(name)
	default:
		return name, nil
	}
}
