/*
 * Minimal object storage library (C) 2015 Minio, Inc.
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

package minio

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// API - object storage API interface
type API interface {
	// Bucket Read/Write/Stat operations
	BucketAPI

	// Object Read/Write/Stat operations
	ObjectAPI
}

// BucketAPI - bucket specific Read/Write/Stat interface
type BucketAPI interface {
	MakeBucket(bucket string, cannedACL BucketACL, location string) error
	BucketExists(bucket string) error
	RemoveBucket(bucket string) error
	SetBucketACL(bucket string, cannedACL BucketACL) error
	GetBucketACL(bucket string) (BucketACL, error)

	ListBuckets() <-chan BucketStatCh
	ListObjects(bucket, prefix string, recursive bool) <-chan ObjectStatCh

	// Drop all incomplete uploads
	DropAllIncompleteUploads(bucket string) <-chan error
}

// ObjectAPI - object specific Read/Write/Stat interface
type ObjectAPI interface {
	GetObject(bucket, object string, offset, length uint64) (io.ReadCloser, ObjectStat, error)
	PutObject(bucket, object string, size uint64, data io.Reader) error
	StatObject(bucket, object string) (ObjectStat, error)
	RemoveObject(bucket, object string) error

	// Drop all incomplete uploads for a given prefix
	DropIncompleteUploads(bucket, prefix string) <-chan error
}

// BucketStatCh - bucket metadata over read channel
type BucketStatCh struct {
	Stat BucketStat
	Err  error
}

// ObjectStatCh - object metadata over read channel
type ObjectStatCh struct {
	Stat ObjectStat
	Err  error
}

// BucketStat container for bucket metadata
type BucketStat struct {
	// The name of the bucket.
	Name string
	// Date the bucket was created.
	CreationDate time.Time
}

// ObjectStat container for object metadata
type ObjectStat struct {
	ETag         string
	Key          string
	LastModified time.Time
	Size         int64

	Owner struct {
		DisplayName string
		ID          string
	}

	// The class of storage used to store the object.
	StorageClass string
}

// Regions s3 region map used by bucket location constraint
var regions = map[string]string{
	"s3-fips-us-gov-west-1.amazonaws.com": "us-gov-west-1",
	"s3.amazonaws.com":                    "us-east-1",
	"s3-us-west-1.amazonaws.com":          "us-west-1",
	"s3-us-west-2.amazonaws.com":          "us-west-2",
	"s3-eu-west-1.amazonaws.com":          "eu-west-1",
	"s3-eu-central-1.amazonaws.com":       "eu-central-1",
	"s3-ap-southeast-1.amazonaws.com":     "ap-southeast-1",
	"s3-ap-southeast-2.amazonaws.com":     "ap-southeast-2",
	"s3-ap-northeast-1.amazonaws.com":     "ap-northeast-1",
	"s3-sa-east-1.amazonaws.com":          "sa-east-1",
	"s3.cn-north-1.amazonaws.com.cn":      "cn-north-1",
}

// getRegion returns a region based on its endpoint mapping.
func getRegion(endPoint string) (region string, err error) {
	u, err := url.Parse(endPoint)
	if err != nil {
		return "", err
	}

	if regions[u.Host] != "" {
		return regions[u.Host], nil
	}

	// Region cannot be empty according to Amazon S3 standard. So
	// we address all the four quadrants of our galaxy.
	return "milkyway", nil
}

// Config - main configuration struct used by all to set endpoint, credentials, and other options for requests.
type Config struct {
	// Standard options
	AccessKeyID     string
	SecretAccessKey string
	Region          string // Optional field. If empty, region is determined automatically.
	Endpoint        string

	// Advanced options
	AcceptType string            // specify this to get server response in non XML style if server supports it
	Transport  http.RoundTripper // custom transport usually for debugging, by default its nil
	// internal
	// use AddUserAgent append to default, useful when minio-go is used with in your application
	userAgent string
}

// Global constants
const (
	LibraryName    = "minio-go"
	LibraryVersion = "0.1"
)

// SetUserAgent - append to a default user agent
func (c *Config) SetUserAgent(name string, version string, comments ...string) {
	// if no name and version is set we do not add new user agents
	if name != "" && version != "" {
		c.userAgent = c.userAgent + " " + name + "/" + version + " (" + strings.Join(comments, ", ") + ") "
	}
}

type api struct {
	lowLevelAPI
}

// New - instantiate a new minio api client
func New(config Config) (API, error) {
	if config.Region == "" {
		region, err := getRegion(config.Endpoint)
		if err != nil {
			return api{}, err
		}
		config.Region = region
	}
	config.SetUserAgent(LibraryName, LibraryVersion, runtime.GOOS, runtime.GOARCH)
	return api{lowLevelAPI{&config}}, nil
}

/// Object operations

// GetObject retrieve object
//
// Additionally it also takes range arguments to download the specified range bytes of an object.
// For more information about the HTTP Range header, go to http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35.
func (a api) GetObject(bucket, object string, offset, length uint64) (io.ReadCloser, ObjectStat, error) {
	if strings.TrimSpace(object) == "" {
		return nil, ObjectStat{}, errors.New("object name cannot be empty")
	}
	if !utf8.ValidString(object) {
		return nil, ObjectStat{}, errors.New("invalid object name, should be utf-8")
	}
	// get the the object
	// NOTE : returned md5sum could be the md5sum of the partial object itself
	// not the whole object depending on if offset range was requested or not
	body, objectMetadata, err := a.getObject(bucket, object, offset, length)
	if err != nil {
		return nil, ObjectStat{}, err
	}
	return body, objectMetadata, nil
}

// completedParts is a wrapper to make parts sortable by their part number
// multi part completion requires list of multi parts to be sorted
type completedParts []completePart

func (a completedParts) Len() int           { return len(a) }
func (a completedParts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a completedParts) Less(i, j int) bool { return a[i].PartNumber < a[j].PartNumber }

// MinimumPartSize minimum part size per object after which PutObject behaves internally as multipart
var MinimumPartSize uint64 = 1024 * 1024 * 5

// maxParts - unexported right now
var maxParts = uint64(10000)

// maxPartSize - unexported right now
var maxPartSize uint64 = 1024 * 1024 * 1024 * 5

// GetPartSize - calculate the optimal part size for the given objectSize
//
// NOTE: Assumption here is that for any given object upload to a S3 compatible object
// storage it will have the following parameters as constants
//
//  maxParts
//  maximumPartSize
//  minimumPartSize
//
// if a the partSize after division with maxParts is greater than MinimumPartSize
// then choose that to be the new part size, if not return MinimumPartSize
//
// special case where it happens to be that partSize is indeed bigger than the
// maximum part size just return maxPartSize back
func GetPartSize(objectSize uint64) uint64 {
	partSize := (objectSize / (maxParts - 1)) // make sure last part has enough buffer and handle this poperly
	{
		if partSize > MinimumPartSize {
			if partSize > maxPartSize {
				return maxPartSize
			}
			return partSize
		}
		return MinimumPartSize
	}
}

func (a api) newObjectUpload(bucket, object string, size uint64, data io.Reader) error {
	initiateMultipartUploadResult, err := a.initiateMultipartUpload(bucket, object)
	if err != nil {
		return err
	}
	uploadID := initiateMultipartUploadResult.UploadID
	completeMultipartUpload := completeMultipartUpload{}
	for part := range multiPart(data, GetPartSize(size), nil) {
		if part.Err != nil {
			return part.Err
		}
		completePart, err := a.uploadPart(bucket, object, uploadID, part.Num, part.Len, part.ReadSeeker)
		if err != nil {
			return err
		}
		completeMultipartUpload.Part = append(completeMultipartUpload.Part, completePart)
	}
	sort.Sort(completedParts(completeMultipartUpload.Part))
	_, err = a.completeMultipartUpload(bucket, object, uploadID, completeMultipartUpload)
	if err != nil {
		return err
	}
	return nil
}

type partCh struct {
	Metadata partMetadata
	Err      error
}

func (a api) listObjectPartsRecursive(bucket, object, uploadID string) <-chan partCh {
	partCh := make(chan partCh)
	go a.listObjectPartsRecursiveInRoutine(bucket, object, uploadID, partCh)
	return partCh
}

func (a api) listObjectPartsRecursiveInRoutine(bucket, object, uploadID string, ch chan partCh) {
	defer close(ch)
	listObjectPartsResult, err := a.listObjectParts(bucket, object, uploadID, 0, 1000)
	if err != nil {
		ch <- partCh{
			Metadata: partMetadata{},
			Err:      err,
		}
		return
	}
	for _, uploadedPart := range listObjectPartsResult.Part {
		ch <- partCh{
			Metadata: uploadedPart,
			Err:      nil,
		}
	}
	for {
		if !listObjectPartsResult.IsTruncated {
			break
		}
		listObjectPartsResult, err = a.listObjectParts(bucket, object, uploadID, listObjectPartsResult.NextPartNumberMarker, 1000)
		if err != nil {
			ch <- partCh{
				Metadata: partMetadata{},
				Err:      err,
			}
			return
		}
		for _, uploadedPart := range listObjectPartsResult.Part {
			ch <- partCh{
				Metadata: uploadedPart,
				Err:      nil,
			}
		}
	}
}

func (a api) continueObjectUpload(bucket, object, uploadID string, size uint64, data io.Reader) error {
	var skipParts []int
	completeMultipartUpload := completeMultipartUpload{}
	for part := range a.listObjectPartsRecursive(bucket, object, uploadID) {
		var completedPart completePart
		completedPart.PartNumber = part.Metadata.PartNumber
		completedPart.ETag = part.Metadata.ETag
		completeMultipartUpload.Part = append(completeMultipartUpload.Part, completedPart)
		skipParts = append(skipParts, part.Metadata.PartNumber)
	}
	for part := range multiPart(data, GetPartSize(size), skipParts) {
		if part.Err != nil {
			return part.Err
		}
		completedPart, err := a.uploadPart(bucket, object, uploadID, part.Num, part.Len, part.ReadSeeker)
		if err != nil {
			return err
		}
		completeMultipartUpload.Part = append(completeMultipartUpload.Part, completedPart)
	}
	sort.Sort(completedParts(completeMultipartUpload.Part))
	_, err := a.completeMultipartUpload(bucket, object, uploadID, completeMultipartUpload)
	if err != nil {
		return err
	}
	return nil
}

type multiPartUploadCh struct {
	Metadata upload
	Err      error
}

func (a api) listMultipartUploadsRecursive(bucket, prefix string) <-chan multiPartUploadCh {
	ch := make(chan multiPartUploadCh)
	go a.listMultipartUploadsRecursiveInRoutine(bucket, prefix, ch)
	return ch
}

func (a api) listMultipartUploadsRecursiveInRoutine(bucket, prefix string, ch chan multiPartUploadCh) {
	defer close(ch)
	listMultipartUploadsResult, err := a.listMultipartUploads(bucket, "", "", prefix, "", 1000)
	if err != nil {
		ch <- multiPartUploadCh{
			Metadata: upload{},
			Err:      err,
		}
		return
	}
	for _, upload := range listMultipartUploadsResult.Upload {
		ch <- multiPartUploadCh{
			Metadata: upload,
			Err:      nil,
		}
	}
	for {
		if !listMultipartUploadsResult.IsTruncated {
			break
		}
		listMultipartUploadsResult, err = a.listMultipartUploads(bucket,
			listMultipartUploadsResult.NextKeyMarker, listMultipartUploadsResult.NextUploadIDMarker, prefix, "", 1000)
		if err != nil {
			ch <- multiPartUploadCh{
				Metadata: upload{},
				Err:      err,
			}
			return
		}
		for _, upload := range listMultipartUploadsResult.Upload {
			ch <- multiPartUploadCh{
				Metadata: upload,
				Err:      nil,
			}
		}
	}
}

// PutObject create an object in a bucket
//
// You must have WRITE permissions on a bucket to create an object
//
// This version of PutObject automatically does multipart for more than 5MB worth of data
func (a api) PutObject(bucket, object string, size uint64, data io.Reader) error {
	if strings.TrimSpace(object) == "" {
		return errors.New("object name cannot be empty")
	}
	if !utf8.ValidString(object) {
		return errors.New("invalid object name, should be utf-8")
	}
	switch {
	case size < MinimumPartSize:
		// Single Part use case, use PutObject directly
		for part := range multiPart(data, MinimumPartSize, nil) {
			if part.Err != nil {
				return part.Err
			}
			_, err := a.putObject(bucket, object, part.Len, part.ReadSeeker)
			if err != nil {
				return err
			}
			return nil
		}
	default:
		var inProgress bool
		var inProgressUploadID string
		for mpUpload := range a.listMultipartUploadsRecursive(bucket, object) {
			if mpUpload.Err != nil {
				return mpUpload.Err
			}
			if mpUpload.Metadata.Key == object {
				inProgress = true
				inProgressUploadID = mpUpload.Metadata.UploadID
				break
			}
		}
		if !inProgress {
			return a.newObjectUpload(bucket, object, size, data)
		}
		return a.continueObjectUpload(bucket, object, inProgressUploadID, size, data)
	}
	return errors.New("Unexpected control flow")
}

// StatObject verify if object exists and you have permission to access it
func (a api) StatObject(bucket, object string) (ObjectStat, error) {
	if strings.TrimSpace(object) == "" {
		return ObjectStat{}, errors.New("object name cannot be empty")
	}
	if !utf8.ValidString(object) {
		return ObjectStat{}, errors.New("invalid object name, should be utf-8")
	}
	return a.headObject(bucket, object)
}

// RemoveObject remove the object from a bucket
func (a api) RemoveObject(bucket, object string) error {
	if strings.TrimSpace(object) == "" {
		return errors.New("object name cannot be empty")
	}
	if !utf8.ValidString(object) {
		return errors.New("invalid object name, should be utf-8")
	}
	return a.deleteObject(bucket, object)
}

/// Bucket operations

// MakeBucket make a new bucket
//
// optional arguments are acl and location - by default all buckets are created
// with ``private`` acl and location set to US Standard if one wishes to set
// different ACLs and Location one can set them properly.
//
// ACL valid values
//
//  private - owner gets full access [default]
//  public-read - owner gets full access, all others get read access
//  public-read-write - owner gets full access, all others get full access too
//  authenticated-read - owner gets full access, authenticated users get read access
//
// Location valid values
//
//  [ us-west-1 | us-west-2 | eu-west-1 | eu-central-1 | ap-southeast-1 | ap-northeast-1 | ap-southeast-2 | sa-east-1 ]
//  Default - US standard
func (a api) MakeBucket(bucket string, acl BucketACL, location string) error {
	if !acl.isValidBucketACL() {
		return fmt.Errorf("%s", "Invalid bucket ACL")
	}
	if _, ok := regions[location]; !ok {
		if location != "" {
			return fmt.Errorf("%s", "Invalid bucket Location")
		}
	}
	return a.putBucket(bucket, string(acl), location)
}

// SetBucketACL set the permissions on an existing bucket using access control lists (ACL)
//
// For example
//
//  private - owner gets full access [default]
//  public-read - owner gets full access, all others get read access
//  public-read-write - owner gets full access, all others get full access too
//  authenticated-read - owner gets full access, authenticated users get read access
//
func (a api) SetBucketACL(bucket string, acl BucketACL) error {
	if !acl.isValidBucketACL() {
		return fmt.Errorf("%s", "Invalid bucket ACL")
	}
	return a.putBucketACL(bucket, string(acl))
}

// GetBucketACL get the permissions on an existing bucket
//
// Returned values are:
//
//  private - owner gets full access
//  public-read - owner gets full access, others get read access
//  public-read-write - owner gets full access, others get full access too
//  authenticated-read - owner gets full access, authenticated users get read access
//
func (a api) GetBucketACL(bucket string) (BucketACL, error) {
	policy, err := a.getBucketACL(bucket)
	if err != nil {
		return "", err
	}
	if policy.AccessControlList.Grant == nil {
		return "", fmt.Errorf("%s", "Unexpected error")
	}
	grants := policy.AccessControlList.Grant
	switch {
	case len(grants) == 1:
		if grants[0].Grantee.URI == "" && grants[0].Permission == "FULL_CONTROL" {
			return BucketACL("private"), nil
		}
	case len(grants) == 2:
		for _, g := range grants {
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AuthenticatedUsers" && g.Permission == "READ" {
				return BucketACL("authenticated-read"), nil
			}
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "READ" {
				return BucketACL("public-read"), nil
			}
		}
	case len(grants) == 3:
		for _, g := range grants {
			if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "WRITE" {
				return BucketACL("public-read-write"), nil
			}
		}
	}
	return "", fmt.Errorf("Cannot verify access control policy")
}

// BucketExists verify if bucket exists and you have permission to access it
func (a api) BucketExists(bucket string) error {
	return a.headBucket(bucket)
}

// RemoveBucket deletes the bucket named in the URI
// NOTE: -
//  All objects (including all object versions and delete markers)
//  in the bucket must be deleted before successfully attempting this request
func (a api) RemoveBucket(bucket string) error {
	return a.deleteBucket(bucket)
}

// listObjectsInRoutine is an internal goroutine function called for listing objects
// This function feeds data into channel
func (a api) listObjectsInRoutine(bucket, prefix string, recursive bool, ch chan ObjectStatCh) {
	defer close(ch)
	switch {
	case recursive == true:
		listBucketResult, err := a.listObjects(bucket, "", prefix, "", 1000)
		if err != nil {
			ch <- ObjectStatCh{
				Stat: ObjectStat{},
				Err:  err,
			}
			return
		}
		for _, object := range listBucketResult.Contents {
			ch <- ObjectStatCh{
				Stat: object,
				Err:  nil,
			}
		}
		for {
			if !listBucketResult.IsTruncated {
				break
			}
			listBucketResult, err = a.listObjects(bucket, listBucketResult.Marker, prefix, "", 1000)
			if err != nil {
				ch <- ObjectStatCh{
					Stat: ObjectStat{},
					Err:  err,
				}
				return
			}
			for _, object := range listBucketResult.Contents {
				ch <- ObjectStatCh{
					Stat: object,
					Err:  nil,
				}
				listBucketResult.Marker = object.Key
			}
		}
	default:
		listBucketResult, err := a.listObjects(bucket, "", prefix, "/", 1000)
		if err != nil {
			ch <- ObjectStatCh{
				Stat: ObjectStat{},
				Err:  err,
			}
			return
		}
		for _, object := range listBucketResult.Contents {
			ch <- ObjectStatCh{
				Stat: object,
				Err:  nil,
			}
		}
		for _, prefix := range listBucketResult.CommonPrefixes {
			object := ObjectStat{}
			object.Key = prefix.Prefix
			object.Size = 0
			ch <- ObjectStatCh{
				Stat: object,
				Err:  nil,
			}
		}
	}
}

// ListObjects - (List Objects) - List some objects or all recursively
//
// ListObjects is a channel based API implemented to facilitate ease of usage of S3 API ListObjects()
// by automatically recursively traversing all objects on a given bucket if specified.
//
// Your input paramters are just bucket, prefix and recursive
//
// If you enable recursive as 'true' this function will return back all the objects in a given bucket
//
//  eg:-
//         api := client.New(....)
//         for message := range api.ListObjects("mytestbucket", "starthere", true) {
//                 fmt.Println(message.Stat)
//         }
//
func (a api) ListObjects(bucket string, prefix string, recursive bool) <-chan ObjectStatCh {
	ch := make(chan ObjectStatCh)
	go a.listObjectsInRoutine(bucket, prefix, recursive, ch)
	return ch
}

// listBucketsInRoutine is an internal go routine function called for listing buckets
// This function feeds data into channel
func (a api) listBucketsInRoutine(ch chan BucketStatCh) {
	defer close(ch)
	listAllMyBucketListResults, err := a.listBuckets()
	if err != nil {
		ch <- BucketStatCh{
			Stat: BucketStat{},
			Err:  err,
		}
		return
	}
	for _, bucket := range listAllMyBucketListResults.Buckets.Bucket {
		ch <- BucketStatCh{
			Stat: bucket,
			Err:  nil,
		}
	}

}

// ListBuckets list of all buckets owned by the authenticated sender of the request
//
// NOTE:
//     This call requires explicit authentication, no anonymous
//     requests are allowed for listing buckets
//
//  eg:-
//         api := client.New(....)
//         for message := range api.ListBuckets() {
//                 fmt.Println(message.Stat)
//         }
//
func (a api) ListBuckets() <-chan BucketStatCh {
	ch := make(chan BucketStatCh)
	go a.listBucketsInRoutine(ch)
	return ch
}

func (a api) dropIncompleteUploadsInRoutine(bucket, prefix string, errorCh chan error) {
	defer close(errorCh)
	listMultipartUploadsResult, err := a.listMultipartUploads(bucket, "", "", prefix, "", 1000)
	if err != nil {
		errorCh <- err
		return
	}
	for _, upload := range listMultipartUploadsResult.Upload {
		err := a.abortMultipartUpload(bucket, upload.Key, upload.UploadID)
		if err != nil {
			errorCh <- err
			return
		}
	}
	for {
		if !listMultipartUploadsResult.IsTruncated {
			break
		}
		listMultipartUploadsResult, err = a.listMultipartUploads(bucket,
			listMultipartUploadsResult.NextKeyMarker, listMultipartUploadsResult.NextUploadIDMarker, prefix, "", 1000)
		if err != nil {
			errorCh <- err
			return
		}
		for _, upload := range listMultipartUploadsResult.Upload {
			err := a.abortMultipartUpload(bucket, upload.Key, upload.UploadID)
			if err != nil {
				errorCh <- err
				return
			}
		}

	}
	errorCh <- nil
}

//
//
// NOTE:
//   These set of calls require explicit authentication, no anonymous
//   requests are allowed for multipart API
//

// DropIncompleteUploads - abort a specific in progress active multipart upload
func (a api) DropIncompleteUploads(bucket, prefix string) <-chan error {
	errorCh := make(chan error)
	go a.dropIncompleteUploadsInRoutine(bucket, prefix, errorCh)
	return errorCh
}

func (a api) dropAllIncompleteUploadsInRoutine(bucket string, errorCh chan error) {
	defer close(errorCh)
	listMultipartUploadsResult, err := a.listMultipartUploads(bucket, "", "", "", "", 1000)
	if err != nil {
		errorCh <- err
		return
	}
	for _, upload := range listMultipartUploadsResult.Upload {
		err := a.abortMultipartUpload(bucket, upload.Key, upload.UploadID)
		if err != nil {
			errorCh <- err
			return
		}
	}
	for {
		if !listMultipartUploadsResult.IsTruncated {
			break
		}
		listMultipartUploadsResult, err = a.listMultipartUploads(bucket,
			listMultipartUploadsResult.NextKeyMarker, listMultipartUploadsResult.NextUploadIDMarker, "", "", 1000)
		if err != nil {
			errorCh <- err
			return
		}
		for _, upload := range listMultipartUploadsResult.Upload {
			err := a.abortMultipartUpload(bucket, upload.Key, upload.UploadID)
			if err != nil {
				errorCh <- err
				return
			}
		}

	}
	errorCh <- nil
}

// DropAllIncompleteUploads - abort all inprogress active multipart uploads
func (a api) DropAllIncompleteUploads(bucket string) <-chan error {
	errorCh := make(chan error)
	go a.dropAllIncompleteUploadsInRoutine(bucket, errorCh)
	return errorCh
}