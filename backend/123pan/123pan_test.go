package _123pan

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/require"
)

// TestIntegration runs the generic backend test suite against a configured
// ordinary 123Pan remote.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName:               "Test123Pan:",
		NilObject:                (*Object)(nil),
		SkipBadWindowsCharacters: true,
		SkipInvalidUTF8:          true,
	})
}

// TestPrepareUploadNoBufferRequiresReopen verifies that no_buffer never
// silently falls back to a disk or memory spool after calculating MD5.
func TestPrepareUploadNoBufferRequiresReopen(t *testing.T) {
	f := &Fs{opt: Options{NoBuffer: true}}
	contents := []byte("cannot reopen")
	src := object.NewStaticObjectInfo("file.txt", time.Now(), int64(len(contents)), true, nil, nil)

	_, err := f.prepareUpload(context.Background(), io.NopCloser(bytes.NewBuffer(contents)), src)
	require.ErrorContains(t, err, "no_buffer requires a seekable or reopenable source")
}

// TestPrepareUploadNoBufferReopensAfterHash verifies that no_buffer hashes
// the supplied stream, then obtains a fresh source for the upload pass.
func TestPrepareUploadNoBufferReopensAfterHash(t *testing.T) {
	contents := []byte("reopen after md5")
	src := &noHashObject{ContentMockObject: mockobject.New("file.txt").WithContent(contents, mockobject.SeekModeNone)}
	f := &Fs{opt: Options{NoBuffer: true}}

	prepared, err := f.prepareUpload(context.Background(), io.NopCloser(bytes.NewBuffer(contents)), src)
	require.NoError(t, err)
	defer prepared.close()
	got, err := io.ReadAll(prepared.reader)
	require.NoError(t, err)
	require.Equal(t, contents, got)
	require.Equal(t, fmt.Sprintf("%x", md5.Sum(contents)), prepared.md5)
	require.Equal(t, 1, src.opens)
}

// TestPrepareUploadUsesKnownMD5WithoutBuffering verifies that the upload path
// does not spool an input when rclone already supplied the required metadata.
func TestPrepareUploadUsesKnownMD5WithoutBuffering(t *testing.T) {
	contents := []byte("known md5")
	reader := &countingReader{Reader: bytes.NewReader(contents)}
	src := object.NewStaticObjectInfo("file.txt", time.Now(), int64(len(contents)), true, map[hash.Type]string{
		hash.MD5: fmt.Sprintf("%x", md5.Sum(contents)),
	}, nil)
	f := &Fs{opt: Options{HashMemoryLimit: defaultHashMemoryLimit}}

	prepared, err := f.prepareUpload(context.Background(), reader, src)
	require.NoError(t, err)
	defer prepared.close()
	require.Same(t, reader, prepared.reader)
	require.Zero(t, reader.reads)
}

// TestDirCacheFlushResetsCachedDirectories verifies that callers can discard
// cached 123Pan directory IDs after an out-of-band change.
func TestDirCacheFlushResetsCachedDirectories(t *testing.T) {
	f := &Fs{dirCache: dircache.New("", rootID, nil)}
	f.dirCache.Put("directory", "42")

	f.DirCacheFlush()

	_, found := f.dirCache.Get("directory")
	require.False(t, found)
}

// TestNewFsLoadsPersistedSessionToken verifies that a later rclone process can
// use the web session saved by an earlier one without signing in again.
func TestNewFsLoadsPersistedSessionToken(t *testing.T) {
	const token = "persisted-session"
	m := configmap.Simple{
		"username":         "user",
		"password":         obscure.MustObscure("password"),
		config.ConfigToken: token,
	}

	got, err := NewFs(context.Background(), "remote", "", m)
	require.NoError(t, err)
	require.Equal(t, token, got.(*Fs).accessToken)
}

// TestPutUsesOrdinaryPreSignedUpload verifies the ordinary upload-request,
// temporary S3 URL, direct PUT, and completion sequence.
func TestPutUsesOrdinaryPreSignedUpload(t *testing.T) {
	contents := []byte("123Pan temporary S3 payload")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/presigned" {
			assertOrdinaryAPIRequest(t, request)
		}
		switch request.URL.Path {
		case "/b/api/file/upload_request":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, "file.txt", body["fileName"])
			require.Equal(t, float64(len(contents)), body["size"])
			require.Equal(t, fmt.Sprintf("%x", md5.Sum(contents)), body["etag"])
			_, _ = response.Write([]byte(`{"code":0,"data":{"FileId":42,"Bucket":"bucket","Key":"object-key","StorageNode":"node","UploadId":"upload-id"}}`))
		case "/b/api/file/s3_upload_object/auth":
			_, _ = response.Write([]byte(`{"code":0,"data":{"presignedUrls":{"1":"` + server.URL + `/presigned"}}}`))
		case "/presigned":
			require.Equal(t, http.MethodPut, request.Method)
			part, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, contents, part)
		case "/b/api/file/upload_complete/v2":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, float64(42), body["fileId"])
			require.Equal(t, false, body["isMultipart"])
			_, _ = response.Write([]byte(`{"code":0}`))
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	src := object.NewStaticObjectInfo("file.txt", time.Now(), int64(len(contents)), true, nil, nil)
	stored, err := f.Put(context.Background(), bytes.NewReader(contents), src)
	require.NoError(t, err)
	require.Equal(t, "42", stored.(*Object).ID())
}

// TestPutUsesTemporaryS3Credentials verifies that temporary S3 credentials use
// rclone's HTTP client, upload the object, and complete the ordinary API flow.
func TestPutUsesTemporaryS3Credentials(t *testing.T) {
	contents := []byte("123Pan temporary S3 credential payload")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/b/api/file/upload_request":
			assertOrdinaryAPIRequest(t, request)
			_, _ = response.Write([]byte(`{"code":0,"data":{"FileId":42,"Bucket":"bucket","Key":"object-key","AccessKeyId":"access-key","SecretAccessKey":"secret-key","SessionToken":"session-token","EndPoint":"` + server.URL + `"}}`))
		case "/bucket/object-key":
			require.Equal(t, http.MethodPut, request.Method)
			require.NotEmpty(t, request.Header.Get("Authorization"))
			part, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, contents, part)
		case "/b/api/file/upload_complete":
			assertOrdinaryAPIRequest(t, request)
			_, _ = response.Write([]byte(`{"code":0}`))
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	src := object.NewStaticObjectInfo("file.txt", time.Now(), int64(len(contents)), true, map[hash.Type]string{
		hash.MD5: fmt.Sprintf("%x", md5.Sum(contents)),
	}, nil)
	stored, err := f.Put(context.Background(), bytes.NewReader(contents), src)
	require.NoError(t, err)
	require.Equal(t, "42", stored.(*Object).ID())
}

// TestPreSignedUploadDoesNotRetryConsumedPart verifies that an upload URL is
// not retried after its input stream has been consumed.
func TestPreSignedUploadDoesNotRetryConsumedPart(t *testing.T) {
	contents := []byte("cannot retry this part")
	var attempts int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/presigned" {
			assertOrdinaryAPIRequest(t, request)
		}
		switch request.URL.Path {
		case "/b/api/file/upload_request":
			_, _ = response.Write([]byte(`{"code":0,"data":{"FileId":42,"Bucket":"bucket","Key":"object-key","StorageNode":"node","UploadId":"upload-id"}}`))
		case "/b/api/file/s3_upload_object/auth":
			_, _ = response.Write([]byte(`{"code":0,"data":{"presignedUrls":{"1":"` + server.URL + `/presigned"}}}`))
		case "/presigned":
			attempts++
			http.Error(response, "temporary failure", http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	src := object.NewStaticObjectInfo("file.txt", time.Now(), int64(len(contents)), true, nil, nil)
	_, err := f.Put(context.Background(), bytes.NewReader(contents), src)
	require.Error(t, err)
	require.Equal(t, 1, attempts)
}

// TestMoveUsesOrdinaryMoveAPI verifies that Move remains a server-side ordinary
// API operation.
func TestMoveUsesOrdinaryMoveAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/b/api/file/mod_pid", request.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, float64(9), body["parentFileId"])
		fileIDs := body["fileIdList"].([]any)
		require.Equal(t, float64(42), fileIDs[0].(map[string]any)["FileId"])
		_, _ = response.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.dirCache.Put("destination", "9")
	source := &Object{fs: f, remote: "source.mp4", id: 42, parent: 1, size: 12, s3Key: "download-key"}

	moved, err := f.Move(context.Background(), source, "destination/source.mp4")
	require.NoError(t, err)
	require.Equal(t, "42", moved.(*Object).ID())
	require.Equal(t, "download-key", moved.(*Object).s3Key)
}

// TestCopyUsesOrdinaryAsyncCopyAPI verifies that Copy submits the ordinary
// 123Pan asynchronous copy task, waits for it, then resolves the new object.
func TestCopyUsesOrdinaryAsyncCopyAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		switch request.URL.Path {
		case "/b/api/restful/goapi/v1/file/copy/async":
			require.Equal(t, http.MethodPost, request.Method)
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, float64(9), body["targetFileId"])
			files := body["fileList"].([]any)
			require.Len(t, files, 1)
			file := files[0].(map[string]any)
			require.Equal(t, float64(42), file["fileId"])
			require.Equal(t, float64(1), file["parentFileId"])
			require.Equal(t, "source.mp4", file["fileName"])
			require.Equal(t, "229739d7be24658bb3710cf604a02aaa", file["etag"])
			_, _ = response.Write([]byte(`{"code":0,"data":{"taskId":1002740,"mode":1}}`))
		case "/b/api/restful/goapi/v1/file/copy/task":
			require.Equal(t, "1002740", request.URL.Query().Get("taskId"))
			_, _ = response.Write([]byte(`{"code":0,"data":{"taskId":1002740,"status":2,"errorCode":0}}`))
		case "/b/api/file/list/new":
			require.Equal(t, "9", request.URL.Query().Get("parentFileId"))
			_, _ = response.Write([]byte(`{"code":0,"data":{"Next":"-1","Total":1,"InfoList":[{"FileName":"source.mp4","Size":12,"UpdateAt":"2026-07-25 02:50:36","FileId":43,"ParentFileId":9,"Type":0,"Etag":"229739d7be24658bb3710cf604a02aaa"}]}}`))
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.dirCache.Put("destination", "9")
	sourceFs := &Fs{name: f.name}
	source := &Object{
		fs:      sourceFs,
		remote:  "source.mp4",
		id:      42,
		parent:  1,
		size:    12,
		md5sum:  "229739d7be24658bb3710cf604a02aaa",
		modTime: time.Date(2026, time.July, 25, 2, 50, 36, 0, time.FixedZone("UTC+8", 8*60*60)),
	}

	copy, err := f.Copy(context.Background(), source, "destination/source.mp4")
	require.NoError(t, err)
	require.Equal(t, "43", copy.(*Object).ID())
}

// TestListUsesStandardFilenameEncoding verifies that provider names are
// exposed through rclone's standard representation for forbidden characters.
func TestListUsesStandardFilenameEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, "/b/api/file/list/new", request.URL.Path)
		_, _ = response.Write([]byte(`{"code":0,"data":{"Next":"-1","Total":1,"InfoList":[{"FileName":"name：with-colon","Size":1,"FileId":42,"ParentFileId":0,"Type":0}]}}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.opt.Enc = defaultEncoding
	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "name:with-colon", entries[0].Remote())
}

// TestListUsesRequestedParentID verifies that the list directory, rather than
// an optional response field, supplies the parent used by copy requests.
func TestListUsesRequestedParentID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, "/b/api/file/list/new", request.URL.Path)
		require.Equal(t, "9", request.URL.Query().Get("parentFileId"))
		_, _ = response.Write([]byte(`{"code":0,"data":{"Next":"-1","Total":1,"InfoList":[{"FileName":"file.txt","Size":1,"FileId":42,"Type":0}]}}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.dirCache.Put("directory", "9")
	entries, err := f.List(context.Background(), "directory")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "9", entries[0].(*Object).ParentID())
}

// TestOpenUsesOrdinaryDownloadAPI verifies that Open obtains and follows an
// ordinary API download URL with the provider's required referrer.
func TestOpenUsesOrdinaryDownloadAPI(t *testing.T) {
	contents := []byte("downloaded contents")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/b/api/file/download_info":
			assertOrdinaryAPIRequest(t, request)
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, float64(42), body["fileId"])
			_, _ = response.Write([]byte(`{"code":0,"data":{"DownloadUrl":"` + server.URL + `/download"}}`))
		case "/download":
			require.Equal(t, http.MethodGet, request.Method)
			require.Equal(t, server.URL+"/", request.Header.Get("Referer"))
			_, _ = response.Write(contents)
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	o := &Object{fs: f, remote: "file.txt", id: 42, parent: 0, size: int64(len(contents)), md5sum: "etag"}
	reader, err := o.Open(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, contents, got)
}

// TestMkdirUsesOrdinaryUploadRequest verifies that directory creation uses the
// ordinary upload-request endpoint with type one.
func TestMkdirUsesOrdinaryUploadRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		switch request.URL.Path {
		case "/b/api/file/list/new":
			_, _ = response.Write([]byte(`{"code":0,"data":{"Next":"-1","Total":0,"InfoList":[]}}`))
		case "/b/api/file/upload_request":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, "directory", body["fileName"])
			require.Equal(t, float64(1), body["type"])
			_, _ = response.Write([]byte(`{"code":0,"data":{"FileId":9}}`))
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	require.NoError(t, f.Mkdir(context.Background(), "directory"))
}

// TestMoveRenamesWithinTheSameDirectory verifies that Move uses the ordinary
// rename endpoint when only the leaf name changes.
func TestMoveRenamesWithinTheSameDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, "/b/api/file/rename", request.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, float64(42), body["fileId"])
		require.Equal(t, "renamed.mp4", body["fileName"])
		_, _ = response.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	source := &Object{fs: f, remote: "source.mp4", id: 42, parent: 0}
	moved, err := f.Move(context.Background(), source, "renamed.mp4")
	require.NoError(t, err)
	require.Equal(t, "renamed.mp4", moved.Remote())
}

// TestRemoveUsesOrdinaryRecycleAPI verifies that object removal moves the
// object to the ordinary 123Pan recycle bin.
func TestRemoveUsesOrdinaryRecycleAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, "/b/api/file/trash", request.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		files := body["fileTrashInfoList"].([]any)
		require.Equal(t, float64(42), files[0].(map[string]any)["FileId"])
		_, _ = response.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	o := &Object{fs: f, remote: "file.txt", id: 42, parent: 0}
	require.NoError(t, o.Remove(context.Background()))
}

// TestAboutUsesOrdinaryUserInfo verifies that About maps ordinary account
// capacity fields to rclone usage values.
func TestAboutUsesOrdinaryUserInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertOrdinaryAPIRequest(t, request)
		require.Equal(t, "/b/api/user/info", request.URL.Path)
		_, _ = response.Write([]byte(`{"code":0,"data":{"SpaceUsed":5,"SpacePermanent":7,"SpaceTemp":11}}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	usage, err := f.About(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(5), *usage.Used)
	require.Equal(t, int64(18), *usage.Total)
}

// TestCallJSONRelogsOnceForAccessTokenFailure verifies that an ordinary API
// 401 establishes one new session and does not create a retry loop.
func TestCallJSONRelogsOnceForAccessTokenFailure(t *testing.T) {
	var apiRequests, logins int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/b/api/user/info":
			assertOrdinaryAPIRequest(t, request)
			apiRequests++
			if apiRequests == 1 {
				require.Equal(t, "Bearer access-1", request.Header.Get("Authorization"))
				_, _ = response.Write([]byte(`{"code":401,"message":"access token invalid"}`))
				return
			}
			require.Equal(t, "Bearer access-2", request.Header.Get("Authorization"))
			_, _ = response.Write([]byte(`{"code":0,"data":{"SpaceUsed":1,"SpacePermanent":2,"SpaceTemp":3}}`))
		case "/user/sign_in":
			logins++
			require.Empty(t, request.Header.Get("Authorization"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, "user", body["passport"])
			require.Equal(t, "password", body["password"])
			require.Equal(t, true, body["remember"])
			_, _ = response.Write([]byte(`{"code":200,"data":{"token":"access-2"}}`))
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\nusername = user\npassword = "+obscure.MustObscure("password")+"\n"), 0o600))
	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	t.Cleanup(func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	})

	f := newAPITestFs(t, server)
	f.sessionSection = "remote"
	response := new(api.UserInfoResponse)
	err := f.callJSON(context.Background(), &rest.Opts{Method: http.MethodGet, Path: "/user/info"}, nil, response)
	require.NoError(t, err)
	require.Equal(t, 2, apiRequests)
	require.Equal(t, 1, logins)
	stored, found := config.FileGetValue("remote", config.ConfigToken)
	require.True(t, found)
	require.Equal(t, "access-2", stored)

	regInfo, err := fs.Find("123pan")
	require.NoError(t, err)
	got, err := NewFs(context.Background(), "remote", "", fs.ConfigMap(regInfo.Prefix, regInfo.Options, "remote", nil))
	require.NoError(t, err)
	require.Equal(t, "access-2", got.(*Fs).accessToken)
}

func assertOrdinaryAPIRequest(t *testing.T, request *http.Request) {
	t.Helper()
	require.Equal(t, "web", request.Header.Get("Platform"))
	foundSignature := false
	for _, values := range request.URL.Query() {
		for _, value := range values {
			if len(strings.Split(value, "-")) == 3 {
				foundSignature = true
			}
		}
	}
	require.True(t, foundSignature, "ordinary 123Pan request is missing its signed query parameter")
}

func newAPITestFs(t *testing.T, server *httptest.Server) *Fs {
	t.Helper()
	f := &Fs{
		name: "remote",
		opt: Options{
			Username:        "user",
			Password:        obscure.MustObscure("password"),
			Platform:        defaultPlatform,
			HashMemoryLimit: defaultHashMemoryLimit,
		},
		srv:         rest.NewClient(server.Client()).SetRoot(server.URL + apiPathPrefix),
		loginSrv:    rest.NewClient(server.Client()).SetRoot(server.URL),
		downloadSrv: rest.NewClient(server.Client()),
		pacer:       fs.NewPacer(context.Background(), pacer.NewDefault(pacer.MinSleep(0), pacer.MaxSleep(time.Millisecond))),
		accessToken: "access-1",
		generation:  1,
		copyDelay:   0,
	}
	f.dirCache = dircache.New("", rootID, f)
	require.NoError(t, f.dirCache.FindRoot(context.Background(), false))
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(context.Background(), f)
	return f
}

type noHashObject struct {
	*mockobject.ContentMockObject
	opens int
}

type countingReader struct {
	io.Reader
	reads int
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	r.reads++
	return r.Reader.Read(buffer)
}

func (o *noHashObject) Hash(context.Context, hash.Type) (string, error) {
	return "", nil
}

func (o *noHashObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	o.opens++
	return o.ContentMockObject.Open(ctx, options...)
}
