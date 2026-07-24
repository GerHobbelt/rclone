// Package api contains types used by the 123Pan Open API.
package api

import (
	"fmt"
)

// Response is embedded in every 123Pan Open API response.
type Response struct {
	// Code is the Open API status code.
	Code int `json:"code"`
	// Message is the provider's status text.
	Message string `json:"message"`
	// XTraceID identifies the provider request for support diagnostics.
	XTraceID string `json:"x-traceID"`
}

// Err returns an error when the Open API response was unsuccessful.
func (r *Response) Err() error {
	if r.Code == 0 {
		return nil
	}
	if r.Message == "" {
		return fmt.Errorf("123Pan API error %d", r.Code)
	}
	return fmt.Errorf("123Pan API error %d: %s", r.Code, r.Message)
}

// IsAuthenticationFailure reports whether the response asks the caller to
// obtain a new access token.
func (r *Response) IsAuthenticationFailure() bool {
	return r.Code == 401
}

// File is a file or directory returned by the list API.
type File struct {
	// FileID is the provider file identifier.
	FileID int64 `json:"fileId"`
	// Filename is the unencoded file or directory name.
	Filename string `json:"filename"`
	// ParentFileID is the parent directory identifier.
	ParentFileID int64 `json:"parentFileId"`
	// Type is zero for a file and one for a directory.
	Type int `json:"type"`
	// Size is the byte size of a file.
	Size int64 `json:"size"`
	// ETag is the file MD5 supplied by the Open API.
	ETag string `json:"etag"`
	// Trashed is nonzero when the item is in the recycle bin.
	Trashed int `json:"trashed"`
	// CreateAt is the provider creation timestamp.
	CreateAt string `json:"createAt"`
	// UpdateAt is the provider modification timestamp.
	UpdateAt string `json:"updateAt"`
}

// FileListResponse is returned by the v2 file-list API.
type FileListResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the page of files.
	Data struct {
		// LastFileID is the cursor for the following page or -1 at the end.
		LastFileID int64 `json:"lastFileId"`
		// FileList is the returned entries.
		FileList []File `json:"fileList"`
	} `json:"data"`
}

// DownloadInfoResponse is returned by the download-info API.
type DownloadInfoResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the temporary download location.
	Data struct {
		// DownloadURL is the temporary URL for the requested object.
		DownloadURL string `json:"downloadUrl"`
	} `json:"data"`
}

// MkdirResponse is returned by the mkdir API.
type MkdirResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the created directory information.
	Data struct {
		// DirID is the created directory identifier.
		DirID int64 `json:"dirID"`
	} `json:"data"`
}

// CopyResponse is returned by the single-file copy API.
type CopyResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the source and target identifiers.
	Data struct {
		// SourceFileID is the copied source object identifier.
		SourceFileID int64 `json:"sourceFileId"`
		// TargetFileID is the new target object identifier.
		TargetFileID int64 `json:"targetFileId"`
	} `json:"data"`
}

// UserInfoResponse is returned by the user-info API.
type UserInfoResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains account space totals.
	Data struct {
		// SpaceUsed is the bytes currently in use.
		SpaceUsed int64 `json:"spaceUsed"`
		// SpacePermanent is the permanent storage quota in bytes.
		SpacePermanent int64 `json:"spacePermanent"`
		// SpaceTemp is the temporary storage quota in bytes.
		SpaceTemp int64 `json:"spaceTemp"`
	} `json:"data"`
}

// UploadCreateResponse is returned by the v2 upload-create API.
type UploadCreateResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the multipart upload plan.
	Data struct {
		// FileID is the created or reusable file identifier.
		FileID int64 `json:"fileID"`
		// PreuploadID identifies the multipart upload session.
		PreuploadID string `json:"preuploadID"`
		// Reuse reports whether the provider deduplicated the content.
		Reuse bool `json:"reuse"`
		// SliceSize is the required byte size of each uploaded slice.
		SliceSize int64 `json:"sliceSize"`
		// Servers is the accepted slice-upload server list.
		Servers []string `json:"servers"`
	} `json:"data"`
}

// UploadCompleteResponse is returned by the v2 upload-complete API.
type UploadCompleteResponse struct {
	// Response contains the common Open API status.
	Response
	// Data contains the completion result.
	Data struct {
		// Completed reports whether the provider finalized the upload.
		Completed bool `json:"completed"`
		// FileID is the finalized object identifier.
		FileID int64 `json:"fileID"`
	} `json:"data"`
}
