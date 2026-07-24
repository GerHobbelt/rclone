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
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// TestIntegration runs the generic backend test suite against a configured
// 123Pan remote.
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

// TestPrepareUploadUsesKnownMD5WithoutBuffering verifies that the ordinary
// upload path does not spool an input when rclone has already supplied both
// pieces of metadata required by the Open API.
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

// TestPutUsesMultipartProtocol verifies the documented create, slice, and
// complete upload sequence, including per-slice MD5s.
func TestPutUsesMultipartProtocol(t *testing.T) {
	contents := []byte("123Pan multipart payload")
	var slices [][]byte
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer access-1", request.Header.Get("Authorization"))
		require.Equal(t, platformHeader, request.Header.Get("Platform"))
		switch request.URL.Path {
		case "/upload/v2/file/create":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, "file.txt", body["filename"])
			require.Equal(t, float64(len(contents)), body["size"])
			require.Equal(t, fmt.Sprintf("%x", md5.Sum(contents)), body["etag"])
			_, _ = response.Write([]byte(`{"code":0,"data":{"reuse":false,"preuploadID":"upload-1","sliceSize":7,"servers":["` + server.URL + `"]}}`))
		case "/upload/v2/file/slice":
			require.NoError(t, request.ParseMultipartForm(1<<20))
			require.Equal(t, "upload-1", request.FormValue("preuploadID"))
			file, _, err := request.FormFile("slice")
			require.NoError(t, err)
			part, err := io.ReadAll(file)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%x", md5.Sum(part)), request.FormValue("sliceMD5"))
			slices = append(slices, part)
			_, _ = response.Write([]byte(`{"code":0}`))
		case "/upload/v2/file/upload_complete":
			_, _ = response.Write([]byte(`{"code":0,"data":{"completed":true,"fileID":42}}`))
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
	require.Equal(t, contents, bytes.Join(slices, nil))
}

// TestNewFsUsesPersistentTokenSection verifies that a runtime suffix is never
// written back as part of the persistent configuration section name.
func TestNewFsUsesPersistentTokenSection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[abc]\ntype = 123pan\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	regInfo, err := fs.Find("123pan")
	require.NoError(t, err)
	mapper := fs.ConfigMap(regInfo.Prefix, regInfo.Options, "abc", nil)
	gotFs, err := NewFs(context.Background(), "abc{A1fie}", "", mapper)
	require.NoError(t, err)
	backend := gotFs.(*Fs)
	defer func() { require.NoError(t, backend.Shutdown(context.Background())) }()

	persisted, ok := config.FileGetValue("abc", fs.ConfigToken)
	require.True(t, ok)
	require.Contains(t, persisted, `"rclone_token_state"`)
	_, suffixed := config.FileGetValue("abc{A1fie}", fs.ConfigToken)
	require.False(t, suffixed)
}

// TestNewFsDoesNotRefreshIdleToken verifies that constructing an unused root
// does not consume a one-time refresh token before an API request needs it.
func TestNewFsDoesNotRefreshIdleToken(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		exchanges.Add(1)
		_, _ = response.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	expired, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntype = 123pan\ntoken_server = "+server.URL+"\ntoken = "+string(expired)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	regInfo, err := fs.Find("123pan")
	require.NoError(t, err)
	gotFs, err := NewFs(context.Background(), "remote", "", fs.ConfigMap(regInfo.Prefix, regInfo.Options, "remote", nil))
	require.NoError(t, err)
	backend := gotFs.(*Fs)
	defer func() { require.NoError(t, backend.Shutdown(context.Background())) }()
	require.Zero(t, exchanges.Load())
}

// TestConfigExchangesFreshRefreshToken verifies that explicit reconnect does
// not persist an unvalidated refresh token as a ready credential.
func TestConfigExchangesFreshRefreshToken(t *testing.T) {
	var exchanges int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "refresh-in", request.URL.Query().Get("refresh_ui"))
		require.Equal(t, "true", request.URL.Query().Get("server_use"))
		require.Equal(t, "123cloud_oa", request.URL.Query().Get("driver_txt"))
		exchanges++
		_, _ = response.Write([]byte(`{"access_token":"access-out","refresh_token":"refresh-out","expires_in":3600}`))
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken_server = "+server.URL+"\n"), 0o600))
	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	defer func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	}()

	_, err := Config(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), fs.ConfigIn{
		State:  "refresh_token",
		Result: "refresh-in",
	})
	require.NoError(t, err)
	require.Equal(t, 1, exchanges)

	persisted, ok := config.FileGetValue("remote", fs.ConfigToken)
	require.True(t, ok)
	require.Contains(t, persisted, `"access_token":"access-out"`)
	require.Contains(t, persisted, `"refresh_token":"refresh-out"`)
	require.Contains(t, persisted, `"status":"ready"`)
}

// TestExchangeTokenClassifiesBrokerBodyRejection verifies that a broker's
// explicit body-level 401 permanently rejects the old rotating credential.
func TestExchangeTokenClassifiesBrokerBodyRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write([]byte(`{"code":401,"message":"refresh token invalid"}`))
	}))
	defer server.Close()

	backend := &Fs{
		opt: Options{TokenServer: server.URL},
		srv: rest.NewClient(server.Client()),
	}
	_, err := backend.exchangeToken(context.Background(), "refresh-1")
	var exchangeErr oauthutil.ExchangeError
	require.ErrorAs(t, err, &exchangeErr)
	require.Equal(t, oauthutil.ExchangeFailureReauthenticationRequired, exchangeErr.ExchangeFailure())
}

// TestRenameUsesDocumentedBatchRenameEndpoint verifies that a single rclone
// rename is encoded through the Open API's batch-rename request shape.
func TestRenameUsesDocumentedBatchRenameEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/api/v1/file/rename", request.URL.Path)
		var body struct {
			RenameList []string `json:"renameList"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, []string{"42|renamed：txt"}, body.RenameList)
		_, _ = response.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.opt.Enc = defaultEncoding
	require.NoError(t, f.rename(context.Background(), 42, "renamed:txt"))
}

// TestListUsesStandardFilenameEncoding verifies that provider names are
// exposed through rclone's standard representation for forbidden characters.
func TestListUsesStandardFilenameEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v2/file/list", request.URL.Path)
		_, _ = response.Write([]byte(`{"code":0,"data":{"lastFileId":-1,"fileList":[{"fileId":42,"filename":"name：with-colon","parentFileId":0,"type":0,"size":1,"trashed":0}]}}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	f.opt.Enc = defaultEncoding
	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "name:with-colon", entries[0].Remote())
}

// TestCallJSONRefreshesOnlyOnceForAccessTokenFailure verifies that a rejected
// request refreshes only its own persisted generation and never loops when the
// replacement access token is rejected too.
func TestCallJSONRefreshesOnlyOnceForAccessTokenFailure(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v1/user/info", request.URL.Path)
		requests++
		_, _ = response.Write([]byte(`{"code":401,"message":"access token invalid"}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	var exchanges int
	source, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges++
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	})
	require.NoError(t, err)
	f.token = source

	response := new(api.UserInfoResponse)
	err = f.callJSON(context.Background(), &rest.Opts{Method: http.MethodGet, Path: "/api/v1/user/info"}, nil, response)
	require.ErrorContains(t, err, "access token invalid")
	require.Equal(t, 2, requests)
	require.Equal(t, 1, exchanges)
}

// TestCallMultipartSharesConditionalRefresh verifies that slice uploads use
// the same one-retry generation protocol as ordinary JSON API calls.
func TestCallMultipartSharesConditionalRefresh(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if requests == 1 {
			require.Equal(t, "Bearer access-1", request.Header.Get("Authorization"))
			_, _ = response.Write([]byte(`{"code":401,"message":"access token invalid"}`))
			return
		}
		require.Equal(t, "Bearer access-2", request.Header.Get("Authorization"))
		_, _ = response.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	f := newAPITestFs(t, server)
	var exchanges int
	source, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", fs.ConfigMap("", nil, "remote", nil), func(context.Context, string) (*oauth2.Token, error) {
		exchanges++
		return &oauth2.Token{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	})
	require.NoError(t, err)
	f.token = source

	err = f.callMultipart(context.Background(), &rest.Opts{Method: http.MethodPost, Path: "/upload/v2/file/slice"}, func() (io.Reader, string, error) {
		return bytes.NewReader([]byte("slice")), "application/octet-stream", nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, requests)
	require.Equal(t, 1, exchanges)
}

func newAPITestFs(t *testing.T, server *httptest.Server) *Fs {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	tokenJSON, err := json.Marshal(&oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte("[remote]\ntoken = "+string(tokenJSON)+"\n"), 0o600))

	oldConfigPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	configfile.Install()
	t.Cleanup(func() {
		require.NoError(t, config.SetConfigPath(oldConfigPath))
		configfile.Install()
	})

	mapper := fs.ConfigMap("", nil, "remote", nil)
	token, err := oauthutil.NewRotatingTokenSource(context.Background(), "remote", mapper, func(context.Context, string) (*oauth2.Token, error) {
		return nil, fmt.Errorf("unexpected token exchange")
	})
	require.NoError(t, err)
	f := &Fs{
		name:        "remote",
		opt:         Options{HashMemoryLimit: defaultHashMemoryLimit},
		srv:         rest.NewClient(server.Client()).SetRoot(server.URL),
		downloadSrv: rest.NewClient(server.Client()),
		pacer:       fs.NewPacer(context.Background(), pacer.NewDefault(pacer.MinSleep(0), pacer.MaxSleep(time.Millisecond))),
		token:       token,
	}
	f.dirCache = dircache.New("", rootID, f)
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
