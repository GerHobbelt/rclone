// Package api contains types used by the ordinary 123Pan web API.
package api

import "fmt"

// Response is embedded in every ordinary 123Pan API response.
type Response struct {
	// Code is the provider status code.
	Code int `json:"code"`
	// Message is the provider status text.
	Message string `json:"message"`
	// XTraceID identifies the provider request for support diagnostics.
	XTraceID string `json:"x-traceID"`
}

// Err returns an error when the ordinary API response was unsuccessful.
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
// establish a new web session.
func (r *Response) IsAuthenticationFailure() bool {
	return r.Code == 401
}

// LoginResponse is returned by the ordinary account sign-in API.
type LoginResponse struct {
	// Code is 200 when the login succeeds.
	Code int `json:"code"`
	// Message is the provider status text.
	Message string `json:"message"`
	// Data contains the web session token.
	Data struct {
		// Token is the authenticated web session token.
		Token string `json:"token"`
	} `json:"data"`
}

// LoginRequest establishes an ordinary 123Pan web session.
type LoginRequest struct {
	// Mail identifies an email-address account.
	Mail string `json:"mail,omitempty"`
	// Passport identifies a phone-number account.
	Passport string `json:"passport,omitempty"`
	// Password is the account password.
	Password string `json:"password"`
	// Type identifies an email-account login.
	Type int `json:"type,omitempty"`
	// Remember asks the provider to retain the phone-account session.
	Remember bool `json:"remember,omitempty"`
}

// UploadRequest creates either a file upload or a directory.
type UploadRequest struct {
	// DriveID identifies the personal drive.
	DriveID int `json:"driveId"`
	// Duplicate controls the provider's file-conflict handling.
	Duplicate int `json:"duplicate,omitempty"`
	// ETag is the complete file MD5, or empty for a directory.
	ETag string `json:"etag"`
	// FileName is the provider-encoded leaf name.
	FileName string `json:"fileName"`
	// ParentFileID identifies the containing directory.
	ParentFileID int64 `json:"parentFileId"`
	// Size is the file byte size, or zero for a directory.
	Size int64 `json:"size"`
	// Type is zero for a file and one for a directory.
	Type int `json:"type"`
}

// TrashRequest moves one or more entries to or from the recycle bin.
type TrashRequest struct {
	// DriveID identifies the personal drive.
	DriveID int `json:"driveId"`
	// Operation selects recycling when true.
	Operation bool `json:"operation"`
	// FileTrashInfoList identifies the entries to recycle.
	FileTrashInfoList []FileID `json:"fileTrashInfoList"`
}

// FileID identifies one ordinary 123Pan file or directory.
type FileID struct {
	// FileID is the provider file identifier.
	FileID int64 `json:"FileId"`
}

// RenameRequest changes an entry's leaf name.
type RenameRequest struct {
	// DriveID identifies the personal drive.
	DriveID int `json:"driveId"`
	// FileID identifies the entry to rename.
	FileID int64 `json:"fileId"`
	// FileName is the provider-encoded replacement leaf name.
	FileName string `json:"fileName"`
}

// MoveRequest changes one or more entries' parent directory.
type MoveRequest struct {
	// FileIDList identifies the entries to move.
	FileIDList []FileID `json:"fileIdList"`
	// ParentFileID identifies the destination directory.
	ParentFileID int64 `json:"parentFileId"`
}

// CopyRequest starts an asynchronous ordinary 123Pan copy task.
type CopyRequest struct {
	// FileList contains the source entries.
	FileList []CopyFile `json:"fileList"`
	// TargetFileID identifies the destination directory.
	TargetFileID int64 `json:"targetFileId"`
}

// CopyFile describes one source entry for an asynchronous copy task.
type CopyFile struct {
	// FileID identifies the source entry.
	FileID int64 `json:"fileId"`
	// Size is the source file byte size.
	Size int64 `json:"size"`
	// ETag is the source file MD5.
	ETag string `json:"etag"`
	// Type is zero for a file and one for a directory.
	Type int `json:"type"`
	// ParentFileID identifies the source directory.
	ParentFileID int64 `json:"parentFileId"`
	// FileName is the provider-encoded source leaf name.
	FileName string `json:"fileName"`
	// DriveID identifies the personal drive.
	DriveID int `json:"driveId"`
}

// DownloadInfoRequest requests a temporary download URL.
type DownloadInfoRequest struct {
	// DriveID identifies the personal drive.
	DriveID int `json:"driveId"`
	// ETag is the requested file MD5.
	ETag string `json:"etag"`
	// FileID identifies the requested file.
	FileID int64 `json:"fileId"`
	// FileName is the provider-encoded file name.
	FileName string `json:"fileName"`
	// S3KeyFlag is supplied by the file-list API.
	S3KeyFlag string `json:"s3keyFlag"`
	// Size is the requested file byte size.
	Size int64 `json:"size"`
	// Type is zero for a file.
	Type int `json:"type"`
}

// S3URLsRequest obtains one or more temporary S3 upload URLs.
type S3URLsRequest struct {
	// StorageNode identifies the temporary upload storage node.
	StorageNode string `json:"StorageNode"`
	// Bucket identifies the temporary S3 bucket.
	Bucket string `json:"bucket"`
	// Key identifies the temporary S3 object.
	Key string `json:"key"`
	// PartNumberEnd is the exclusive upper part-number bound.
	PartNumberEnd int `json:"partNumberEnd"`
	// PartNumberStart is the inclusive lower part-number bound.
	PartNumberStart int `json:"partNumberStart"`
	// UploadID identifies the temporary multipart upload.
	UploadID string `json:"uploadId"`
}

// S3UploadCompleteRequest completes an upload performed with temporary URLs.
type S3UploadCompleteRequest struct {
	// StorageNode identifies the temporary upload storage node.
	StorageNode string `json:"StorageNode"`
	// Bucket identifies the temporary S3 bucket.
	Bucket string `json:"bucket"`
	// FileID identifies the provider file entry.
	FileID int64 `json:"fileId"`
	// FileSize is the uploaded byte count.
	FileSize int64 `json:"fileSize"`
	// IsMultipart reports whether more than one S3 part was uploaded.
	IsMultipart bool `json:"isMultipart"`
	// Key identifies the temporary S3 object.
	Key string `json:"key"`
	// UploadID identifies the temporary multipart upload.
	UploadID string `json:"uploadId"`
}

// UploadCompleteRequest completes an upload performed with temporary S3 credentials.
type UploadCompleteRequest struct {
	// FileID identifies the provider file entry.
	FileID int64 `json:"fileId"`
}

// File is a file or directory returned by the ordinary API.
type File struct {
	// FileName is the unencoded file or directory name.
	FileName string `json:"FileName"`
	// Size is the byte size of a file.
	Size int64 `json:"Size"`
	// UpdateAt is the provider modification timestamp.
	UpdateAt string `json:"UpdateAt"`
	// FileID is the provider file identifier.
	FileID int64 `json:"FileId"`
	// ParentFileID is the provider parent directory identifier.
	ParentFileID int64 `json:"ParentFileId"`
	// Type is zero for a file and one for a directory.
	Type int `json:"Type"`
	// ETag is the file MD5 supplied by 123Pan.
	ETag string `json:"Etag"`
	// S3KeyFlag is required when requesting a download URL.
	S3KeyFlag string `json:"S3KeyFlag"`
}

// FileListResponse is returned by the ordinary file-list API.
type FileListResponse struct {
	// Response contains the common API status.
	Response
	// Data contains a page of files.
	Data struct {
		// Next is -1 when the list has no following page.
		Next string `json:"Next"`
		// Total is the count reported by the provider.
		Total int `json:"Total"`
		// InfoList contains the returned file and directory entries.
		InfoList []File `json:"InfoList"`
	} `json:"data"`
}

// DownloadInfoResponse is returned by the ordinary download-info API.
type DownloadInfoResponse struct {
	// Response contains the common API status.
	Response
	// Data contains the temporary download location.
	Data struct {
		// DownloadURL is the temporary URL for the requested object.
		DownloadURL string `json:"DownloadUrl"`
	} `json:"data"`
}

// UserInfoResponse is returned by the ordinary user-info API.
type UserInfoResponse struct {
	// Response contains the common API status.
	Response
	// Data contains account space totals.
	Data struct {
		// SpaceUsed is the bytes currently in use.
		SpaceUsed int64 `json:"SpaceUsed"`
		// SpacePermanent is the permanent storage quota in bytes.
		SpacePermanent int64 `json:"SpacePermanent"`
		// SpaceTemp is the temporary storage quota in bytes.
		SpaceTemp int64 `json:"SpaceTemp"`
	} `json:"data"`
}

// UploadResponse is returned by the ordinary upload-request API.
type UploadResponse struct {
	// Response contains the common API status.
	Response
	// Data contains the upload plan and object metadata.
	Data struct {
		// AccessKeyID is an optional temporary S3 access key.
		AccessKeyID string `json:"AccessKeyId"`
		// Bucket is the temporary S3 bucket.
		Bucket string `json:"Bucket"`
		// Key is the temporary S3 object key.
		Key string `json:"Key"`
		// SecretAccessKey is an optional temporary S3 secret key.
		SecretAccessKey string `json:"SecretAccessKey"`
		// SessionToken is an optional temporary S3 session token.
		SessionToken string `json:"SessionToken"`
		// FileID is the provider object identifier.
		FileID int64 `json:"FileId"`
		// Reuse reports whether 123Pan deduplicated the content.
		Reuse bool `json:"Reuse"`
		// EndPoint is the temporary S3 endpoint.
		EndPoint string `json:"EndPoint"`
		// StorageNode identifies the temporary upload storage node.
		StorageNode string `json:"StorageNode"`
		// UploadID identifies a multipart temporary S3 upload.
		UploadID string `json:"UploadId"`
	} `json:"data"`
}

// S3PreSignedURLsResponse is returned by the temporary S3 URL APIs.
type S3PreSignedURLsResponse struct {
	// Response contains the common API status.
	Response
	// Data contains URLs keyed by one-based part number.
	Data struct {
		// PreSignedURLs maps each part number to its upload URL.
		PreSignedURLs map[string]string `json:"presignedUrls"`
	} `json:"data"`
}

// CopyStartResponse is returned when the ordinary asynchronous copy task is
// accepted.
type CopyStartResponse struct {
	// Response contains the common API status.
	Response
	// Data contains the asynchronous task identity.
	Data struct {
		// TaskID identifies the copy task.
		TaskID int64 `json:"taskId"`
		// Mode is the provider copy mode.
		Mode int `json:"mode"`
	} `json:"data"`
}

// CopyTaskResponse reports progress and completion of an asynchronous copy.
type CopyTaskResponse struct {
	// Response contains the common API status.
	Response
	// Data contains copy-task status fields.
	Data struct {
		// TaskID identifies the copy task.
		TaskID int64 `json:"taskId"`
		// Status is the provider task state.
		Status int `json:"status"`
		// ErrorCode is nonzero when the task failed.
		ErrorCode int `json:"errorCode"`
		// Reason describes a failed task when supplied.
		Reason string `json:"reason"`
	} `json:"data"`
}
