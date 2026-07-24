// Package _123pan provides an interface to the 123Pan Open API.
package _123pan

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

const (
	apiRootURL             = "https://open-api.123pan.com"
	defaultTokenServer     = "https://api.oplist.org/123cloud/renewapi"
	platformHeader         = "open_platform"
	rootID                 = "0"
	defaultHashMemoryLimit = 16 * fs.Mebi
	maxUploadSize          = 10 * fs.Gibi
	minSleep               = 200 * time.Millisecond
	maxSleep               = 2 * time.Second
	completionAttempts     = 60
	defaultEncoding        = encoder.EncodeSlash |
		encoder.EncodeBackSlash |
		encoder.EncodeColon |
		encoder.EncodeAsterisk |
		encoder.EncodeQuestion |
		encoder.EncodePipe |
		encoder.EncodeLtGt |
		encoder.EncodeDoubleQuote |
		encoder.EncodeCtl |
		encoder.EncodeInvalidUtf8
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "123pan",
		Description: "123Pan Open Platform",
		NewFs:       NewFs,
		Config:      Config,
		Options: []fs.Option{{
			Name:      fs.ConfigToken,
			Help:      "Rotating 123Pan token state. Set automatically by `rclone config reconnect`.",
			Advanced:  true,
			Sensitive: true,
			Hide:      fs.OptionHideBoth,
		}, {
			Name:     "token_server",
			Help:     "Online API used to exchange the rotating refresh token.",
			Default:  defaultTokenServer,
			Advanced: true,
		}, {
			Name:     "hash_memory_limit",
			Help:     "Files at or below this size are kept in memory while calculating the required MD5; larger files use a temporary spool.",
			Default:  defaultHashMemoryLimit,
			Advanced: true,
		}, {
			Name: "no_buffer",
			Help: `Do not spool uploads while calculating MD5.

The source must be seekable or reopenable. rclone returns an error rather than silently buffering when this is not possible.`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default:  defaultEncoding,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	TokenServer     string               `config:"token_server"`
	HashMemoryLimit fs.SizeSuffix        `config:"hash_memory_limit"`
	NoBuffer        bool                 `config:"no_buffer"`
	Enc             encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a 123Pan remote.
type Fs struct {
	name        string
	root        string
	opt         Options
	features    *fs.Features
	srv         *rest.Client
	downloadSrv *rest.Client
	dirCache    *dircache.DirCache
	pacer       *fs.Pacer
	token       *oauthutil.RotatingTokenSource
	renewer     *oauthutil.RotatingRenew
}

// Object represents a 123Pan file.
type Object struct {
	fs      *Fs
	remote  string
	id      int64
	parent  int64
	size    int64
	md5sum  string
	modTime time.Time
}

// Config asks for a fresh rotating refresh token during initial setup and
// explicit reconnect. It deliberately never attempts recovery implicitly.
func Config(ctx context.Context, name string, m configmap.Mapper, configIn fs.ConfigIn) (*fs.ConfigOut, error) {
	switch configIn.State {
	case "":
		if token, ok := m.Get(fs.ConfigToken); ok && token != "" {
			return fs.ConfigConfirm("replace_token", false, "config_replace_token", "Replace the saved 123Pan refresh token?")
		}
		return fs.ConfigInput("refresh_token", "config_refresh_token", "Enter a fresh 123Pan refresh token. It will be sent to the configured token server.")
	case "replace_token":
		if configIn.Result != "true" {
			return nil, nil
		}
		return fs.ConfigInput("refresh_token", "config_refresh_token", "Enter a fresh 123Pan refresh token. It will be sent to the configured token server.")
	case "refresh_token":
		refreshToken := strings.TrimSpace(configIn.Result)
		if refreshToken == "" {
			return fs.ConfigError("", "Refresh token cannot be empty")
		}
		opt := new(Options)
		if err := configstruct.Set(m, opt); err != nil {
			return nil, err
		}
		if opt.TokenServer == "" {
			opt.TokenServer = defaultTokenServer
		}
		backend := &Fs{
			opt: *opt,
			srv: rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiRootURL),
		}
		if _, _, err := oauthutil.ReconnectRotatingToken(ctx, name, m, &oauth2.Token{RefreshToken: refreshToken}, backend.exchangeToken); err != nil {
			return nil, fmt.Errorf("reconnect 123Pan refresh token: %w", err)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown config state %q", configIn.State)
	}
}

// NewFs creates a 123Pan filesystem rooted at root.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	root = strings.Trim(root, "/")
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.TokenServer == "" {
		opt.TokenServer = defaultTokenServer
	}
	f := &Fs{
		name:        name,
		root:        root,
		opt:         *opt,
		srv:         rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiRootURL),
		downloadSrv: rest.NewClient(fshttp.NewClient(ctx)),
		pacer:       fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep))),
	}
	token, err := oauthutil.NewRotatingTokenSource(ctx, name, m, f.exchangeToken)
	if err != nil {
		return nil, fmt.Errorf("123Pan token: %w", err)
	}
	f.token = token
	f.renewer = oauthutil.NewRotatingRenew(ctx, name, token)
	f.dirCache = dircache.New(root, rootID, f)
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if err = f.dirCache.FindRoot(ctx, false); err != nil {
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, rootID, &tempF)
		tempF.root = newRoot
		if err = tempF.dirCache.FindRoot(ctx, false); err != nil {
			return f, nil
		}
		if _, err = tempF.NewObject(ctx, remote); err != nil {
			if err == fs.ErrorObjectNotFound {
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, &tempF)
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// Name returns the configured remote name.
func (f *Fs) Name() string {
	return f.name
}

// Root returns the configured root path.
func (f *Fs) Root() string {
	return f.root
}

// String returns a human-readable description of this remote.
func (f *Fs) String() string {
	return "123Pan root '" + f.root + "'"
}

// Precision returns the precision of modtimes supported by 123Pan.
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the hashes supported by 123Pan.
func (f *Fs) Hashes() hash.Set {
	return hash.NewHashSet(hash.MD5)
}

// Features returns the optional features of this backend.
func (f *Fs) Features() *fs.Features {
	return f.features
}

// DirCacheFlush resets the directory cache after an out-of-band change.
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

type apiResponse interface {
	Err() error
	IsAuthenticationFailure() bool
}

type tokenExchangeResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Code             int    `json:"code"`
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
	Message          string `json:"message"`
	Text             string `json:"text"`
}

// exchangeToken obtains the next access and refresh token from the agreed
// OpenList-compatible online API. Any unclassified failure is deliberately
// left ambiguous for the rotating-token protocol to fail closed.
func (f *Fs) exchangeToken(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	if f.srv == nil {
		return nil, errors.New("123Pan API client is not initialized")
	}
	var response tokenExchangeResponse
	resp, err := f.srv.CallJSON(ctx, &rest.Opts{
		Method:     http.MethodGet,
		RootURL:    f.opt.TokenServer,
		NoRedirect: true,
		// A single-hop fresh connection keeps dial and TLS failures pre-request.
		Close: true,
		Parameters: url.Values{
			"refresh_ui": {refreshToken},
			"server_use": {"true"},
			"driver_txt": {"123cloud_oa"},
		},
	}, nil, &response)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, oauthutil.NewExchangeError(oauthutil.ExchangeFailureReauthenticationRequired, err)
		}
		return nil, oauthutil.ClassifyExchangeTransportError(err)
	}
	message := response.ErrorDescription
	if message == "" {
		message = response.Error
	}
	if message == "" {
		message = response.Message
	}
	if message == "" {
		message = response.Text
	}
	if response.Code == http.StatusUnauthorized || response.Code == http.StatusForbidden {
		if message == "" {
			message = "online token API rejected the refresh token"
		}
		return nil, oauthutil.NewExchangeError(oauthutil.ExchangeFailureReauthenticationRequired, errors.New(message))
	}
	if response.Code != 0 || response.AccessToken == "" || response.RefreshToken == "" || response.ExpiresIn <= 0 {
		if message == "" {
			message = "online token API returned an incomplete token"
		}
		return nil, errors.New(message)
	}
	return &oauth2.Token{
		AccessToken:  response.AccessToken,
		TokenType:    "Bearer",
		RefreshToken: response.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(response.ExpiresIn) * time.Second),
	}, nil
}

func (f *Fs) authOpts(opts *rest.Opts, accessToken string) *rest.Opts {
	copy := *opts
	headers := make(map[string]string, len(opts.ExtraHeaders)+2)
	for key, value := range opts.ExtraHeaders {
		headers[key] = value
	}
	headers["Authorization"] = "Bearer " + accessToken
	headers["Platform"] = platformHeader
	copy.ExtraHeaders = headers
	return &copy
}

func (f *Fs) conditionalRefresh(ctx context.Context, generation uint64) error {
	if f.token == nil {
		return errors.New("rotating token source is not initialized")
	}
	_, _, err := f.token.RefreshIfCurrent(ctx, generation)
	return err
}

// retryAuthenticationFailure refreshes a failed token generation at most once.
func (f *Fs) retryAuthenticationFailure(ctx context.Context, generation uint64, refreshed *bool) (bool, error) {
	if *refreshed {
		return false, nil
	}
	if err := f.conditionalRefresh(ctx, generation); err != nil {
		return false, err
	}
	*refreshed = true
	return true, nil
}

// callJSON calls an authenticated Open API endpoint. A 401 refreshes only the
// generation that issued this request, and it retries the request once.
func (f *Fs) callJSON(ctx context.Context, opts *rest.Opts, request any, response apiResponse) error {
	if f.token == nil {
		return errors.New("rotating token source is not initialized")
	}
	refreshed := false
	return f.pacer.Call(func() (bool, error) {
		token, generation, err := f.token.TokenContext(ctx)
		if err != nil {
			return false, err
		}
		resp, err := f.srv.CallJSON(ctx, f.authOpts(opts, token.AccessToken), request, response)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusUnauthorized {
				retry, refreshErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if refreshErr != nil {
					return false, fmt.Errorf("conditional token refresh: %w", refreshErr)
				}
				if retry {
					return true, nil
				}
			}
			if fserrors.ShouldRetry(err) {
				return true, err
			}
			return false, err
		}
		if err = response.Err(); err != nil {
			if response.IsAuthenticationFailure() {
				retry, refreshErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if refreshErr != nil {
					return false, fmt.Errorf("conditional token refresh: %w", refreshErr)
				}
				if retry {
					return true, nil
				}
			}
			return false, err
		}
		return false, nil
	})
}

func parseFileID(id string) (int64, error) {
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse 123Pan file ID %q: %w", id, err)
	}
	return parsed, nil
}

func parseFileTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	location := time.FixedZone("UTC+8", 8*60*60)
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, location)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (f *Fs) listFiles(ctx context.Context, parentID int64) ([]api.File, error) {
	var files []api.File
	lastFileID := int64(0)
	for pages := 0; ; pages++ {
		if pages >= 100000 {
			return nil, errors.New("too many 123Pan file-list pages")
		}
		var response api.FileListResponse
		err := f.callJSON(ctx, &rest.Opts{
			Method: http.MethodGet,
			Path:   "/api/v2/file/list",
			Parameters: url.Values{
				"parentFileId": {strconv.FormatInt(parentID, 10)},
				"limit":        {"100"},
				"lastFileId":   {strconv.FormatInt(lastFileID, 10)},
			},
		}, nil, &response)
		if err != nil {
			return nil, fmt.Errorf("list 123Pan files: %w", err)
		}
		for _, file := range response.Data.FileList {
			if file.Trashed == 0 {
				files = append(files, file)
			}
		}
		if response.Data.LastFileID == -1 {
			return files, nil
		}
		lastFileID = response.Data.LastFileID
	}
}

func (f *Fs) findFile(ctx context.Context, parentID int64, leaf string) (*api.File, error) {
	files, err := f.listFiles(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for index := range files {
		if f.opt.Enc.ToStandardName(files[index].Filename) == leaf {
			return &files[index], nil
		}
	}
	return nil, nil
}

// FindLeaf finds a child directory with leaf below directoryID.
func (f *Fs) FindLeaf(ctx context.Context, directoryID, leaf string) (string, bool, error) {
	parentID, err := parseFileID(directoryID)
	if err != nil {
		return "", false, err
	}
	file, err := f.findFile(ctx, parentID, leaf)
	if err != nil || file == nil || file.Type != 1 {
		return "", false, err
	}
	return strconv.FormatInt(file.FileID, 10), true, nil
}

// CreateDir creates a directory named leaf below directoryID.
func (f *Fs) CreateDir(ctx context.Context, directoryID, leaf string) (string, error) {
	parentID, err := parseFileID(directoryID)
	if err != nil {
		return "", err
	}
	var response api.MkdirResponse
	err = f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/upload/v1/file/mkdir",
	}, map[string]any{
		"name":     f.opt.Enc.FromStandardName(leaf),
		"parentID": parentID,
	}, &response)
	if err != nil {
		if id, found, findErr := f.FindLeaf(ctx, directoryID, leaf); findErr == nil && found {
			return id, nil
		}
		return "", fmt.Errorf("create 123Pan directory %q: %w", leaf, err)
	}
	if response.Data.DirID == 0 {
		return "", errors.New("123Pan mkdir returned an empty directory ID")
	}
	return strconv.FormatInt(response.Data.DirID, 10), nil
}

// List lists objects and directories below dir.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	parentID, err := parseFileID(directoryID)
	if err != nil {
		return nil, err
	}
	files, err := f.listFiles(ctx, parentID)
	if err != nil {
		return nil, err
	}
	entries := make(fs.DirEntries, 0, len(files))
	for _, file := range files {
		remote := path.Join(dir, f.opt.Enc.ToStandardName(file.Filename))
		if file.Type == 1 {
			id := strconv.FormatInt(file.FileID, 10)
			entries = append(entries, fs.NewDir(remote, parseFileTime(file.UpdateAt)).SetID(id).SetParentID(directoryID))
			f.dirCache.Put(remote, id)
			continue
		}
		entries = append(entries, f.newObject(remote, &file))
	}
	return entries, nil
}

// NewObject finds the file at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	parentID, err := parseFileID(directoryID)
	if err != nil {
		return nil, err
	}
	file, err := f.findFile(ctx, parentID, leaf)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fs.ErrorObjectNotFound
	}
	if file.Type == 1 {
		return nil, fs.ErrorIsDir
	}
	return f.newObject(remote, file), nil
}

// Mkdir makes dir and its missing parents.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

func (f *Fs) removeDir(ctx context.Context, dir string, checkEmpty bool) error {
	if dir == "" {
		return errors.New("can't remove root directory")
	}
	if checkEmpty {
		entries, err := f.List(ctx, dir)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fs.ErrorDirectoryNotEmpty
		}
	}
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	id, err := parseFileID(directoryID)
	if err != nil {
		return err
	}
	if err = f.trash(ctx, id); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// Rmdir moves an empty directory to the 123Pan recycle bin.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.removeDir(ctx, dir, true)
}

// Purge moves a directory and all of its contents to the 123Pan recycle bin.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.removeDir(ctx, dir, false)
}

func (f *Fs) newObject(remote string, file *api.File) *Object {
	return &Object{
		fs:      f,
		remote:  remote,
		id:      file.FileID,
		parent:  file.ParentFileID,
		size:    file.Size,
		md5sum:  strings.ToLower(file.ETag),
		modTime: parseFileTime(file.UpdateAt),
	}
}

// parentDir returns the rclone directory which contains remote.
func parentDir(remote string) string {
	dir, _ := dircache.SplitPath(remote)
	return dir
}

func (f *Fs) trash(ctx context.Context, id int64) error {
	response := new(api.Response)
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/api/v1/file/trash",
	}, map[string]any{"fileIDs": []int64{id}}, response)
	if err != nil {
		return fmt.Errorf("move 123Pan file to recycle bin: %w", err)
	}
	return nil
}

func (f *Fs) rename(ctx context.Context, id int64, name string) error {
	response := new(api.Response)
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/api/v1/file/rename",
	}, map[string]any{"renameList": []string{strconv.FormatInt(id, 10) + "|" + f.opt.Enc.FromStandardName(name)}}, response)
	if err != nil {
		return fmt.Errorf("rename 123Pan file: %w", err)
	}
	return nil
}

func (f *Fs) move(ctx context.Context, id, parentID int64) error {
	response := new(api.Response)
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/api/v1/file/move",
	}, map[string]any{"fileIDs": []int64{id}, "toParentFileID": parentID}, response)
	if err != nil {
		return fmt.Errorf("move 123Pan file: %w", err)
	}
	return nil
}

func (f *Fs) copy(ctx context.Context, id, parentID int64) (int64, error) {
	var response api.CopyResponse
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/api/v1/file/copy",
	}, map[string]any{"fileId": id, "targetDirId": parentID}, &response)
	if err != nil {
		return 0, fmt.Errorf("copy 123Pan file: %w", err)
	}
	if response.Data.TargetFileID == 0 {
		return 0, errors.New("123Pan copy returned an empty target file ID")
	}
	return response.Data.TargetFileID, nil
}

// Fs returns the filesystem that contains o.
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String returns a human-readable description of o.
func (o *Object) String() string {
	return o.remote
}

// Remote returns o's remote path.
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns o's modification time.
func (o *Object) ModTime(context.Context) time.Time {
	return o.modTime
}

// Size returns o's size.
func (o *Object) Size() int64 {
	return o.size
}

// Storable reports whether o can be stored.
func (o *Object) Storable() bool {
	return true
}

// Hash returns o's MD5 checksum.
func (o *Object) Hash(_ context.Context, hashType hash.Type) (string, error) {
	if hashType != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5sum, nil
}

// ID returns the 123Pan file ID.
func (o *Object) ID() string {
	return strconv.FormatInt(o.id, 10)
}

// ParentID returns the 123Pan parent directory ID.
func (o *Object) ParentID() string {
	return strconv.FormatInt(o.parent, 10)
}

// SetModTime reports that the Open API does not support setting modtime.
func (o *Object) SetModTime(context.Context, time.Time) error {
	return fs.ErrorCantSetModTime
}

// Remove moves o to the 123Pan recycle bin.
func (o *Object) Remove(ctx context.Context) error {
	if err := o.fs.trash(ctx, o.id); err != nil {
		return err
	}
	o.fs.dirCache.FlushDir(parentDir(o.remote))
	return nil
}

// Open opens o for download.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	var info api.DownloadInfoResponse
	if err := o.fs.callJSON(ctx, &rest.Opts{
		Method: http.MethodGet,
		Path:   "/api/v1/file/download_info",
		Parameters: url.Values{
			"fileId": {strconv.FormatInt(o.id, 10)},
		},
	}, nil, &info); err != nil {
		return nil, fmt.Errorf("get 123Pan download URL: %w", err)
	}
	if info.Data.DownloadURL == "" {
		return nil, errors.New("123Pan returned an empty download URL")
	}
	var response *http.Response
	err := o.fs.pacer.Call(func() (bool, error) {
		var err error
		response, err = o.fs.downloadSrv.Call(ctx, &rest.Opts{
			Method:  http.MethodGet,
			RootURL: info.Data.DownloadURL,
			Options: options,
		})
		if err != nil && fserrors.ShouldRetry(err) {
			return true, err
		}
		return false, err
	})
	if err != nil {
		return nil, fmt.Errorf("open 123Pan file: %w", err)
	}
	return response.Body, nil
}

func (f *Fs) callMultipart(ctx context.Context, opts *rest.Opts, makeBody func() (io.Reader, string, error)) error {
	if f.token == nil {
		return errors.New("rotating token source is not initialized")
	}
	refreshed := false
	return f.pacer.Call(func() (bool, error) {
		token, generation, err := f.token.TokenContext(ctx)
		if err != nil {
			return false, err
		}
		body, contentType, err := makeBody()
		if err != nil {
			return false, err
		}
		callOpts := f.authOpts(opts, token.AccessToken)
		callOpts.Body = body
		callOpts.ContentType = contentType
		resp, err := f.srv.Call(ctx, callOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusUnauthorized {
				retry, refreshErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if refreshErr != nil {
					return false, fmt.Errorf("conditional token refresh: %w", refreshErr)
				}
				if retry {
					return true, nil
				}
			}
			if fserrors.ShouldRetry(err) {
				return true, err
			}
			return false, err
		}
		var response api.Response
		if err = rest.DecodeJSON(resp, &response); err != nil {
			return false, err
		}
		if err = response.Err(); err != nil {
			if response.IsAuthenticationFailure() {
				retry, refreshErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if refreshErr != nil {
					return false, fmt.Errorf("conditional token refresh: %w", refreshErr)
				}
				if retry {
					return true, nil
				}
			}
			return false, err
		}
		return false, nil
	})
}

func (f *Fs) createUpload(ctx context.Context, parentID int64, leaf, md5sum string, size int64) (*api.UploadCreateResponse, error) {
	var response api.UploadCreateResponse
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/upload/v2/file/create",
	}, map[string]any{
		"parentFileID": parentID,
		"filename":     f.opt.Enc.FromStandardName(leaf),
		"etag":         strings.ToLower(md5sum),
		"size":         size,
		"duplicate":    2,
		"containDir":   false,
	}, &response)
	if err != nil {
		return nil, fmt.Errorf("create 123Pan upload: %w", err)
	}
	return &response, nil
}

func makeSliceBody(preuploadID string, number int, filename string, contents []byte) (io.Reader, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("preuploadID", preuploadID); err != nil {
		return nil, "", err
	}
	if err := writer.WriteField("sliceNo", strconv.Itoa(number)); err != nil {
		return nil, "", err
	}
	if err := writer.WriteField("sliceMD5", fmt.Sprintf("%x", md5.Sum(contents))); err != nil {
		return nil, "", err
	}
	part, err := rest.CreateFormFile(writer, "slice", filename+".part"+strconv.Itoa(number), "application/octet-stream")
	if err != nil {
		return nil, "", err
	}
	if _, err = part.Write(contents); err != nil {
		return nil, "", err
	}
	if err = writer.Close(); err != nil {
		return nil, "", err
	}
	return bytes.NewReader(buffer.Bytes()), writer.FormDataContentType(), nil
}

func (f *Fs) uploadSlice(ctx context.Context, server, preuploadID string, number int, filename string, contents []byte) error {
	server = strings.TrimRight(server, "/")
	if server == "" {
		return errors.New("123Pan upload server is empty")
	}
	err := f.callMultipart(ctx, &rest.Opts{
		Method:  http.MethodPost,
		RootURL: server,
		Path:    "/upload/v2/file/slice",
	}, func() (io.Reader, string, error) {
		return makeSliceBody(preuploadID, number, f.opt.Enc.FromStandardName(filename), contents)
	})
	if err != nil {
		return fmt.Errorf("upload 123Pan slice %d: %w", number, err)
	}
	return nil
}

func (f *Fs) completeUpload(ctx context.Context, preuploadID string) (int64, error) {
	for attempt := 0; attempt < completionAttempts; attempt++ {
		var response api.UploadCompleteResponse
		err := f.callJSON(ctx, &rest.Opts{
			Method: http.MethodPost,
			Path:   "/upload/v2/file/upload_complete",
		}, map[string]any{"preuploadID": preuploadID}, &response)
		if err != nil {
			return 0, fmt.Errorf("complete 123Pan upload: %w", err)
		}
		if response.Data.Completed && response.Data.FileID != 0 {
			return response.Data.FileID, nil
		}
		if attempt+1 < completionAttempts {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return 0, errors.New("123Pan upload did not complete before the polling deadline")
}

func (f *Fs) put(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (*Object, error) {
	prepared, err := f.prepareUpload(ctx, in, src, options...)
	if err != nil {
		return nil, err
	}
	defer prepared.close()
	if prepared.size < 0 {
		return nil, errors.New("123Pan uploads require a known size")
	}
	if prepared.size > int64(maxUploadSize) {
		return nil, fmt.Errorf("123Pan upload size %d exceeds the Open API limit of %d bytes", prepared.size, maxUploadSize)
	}
	leaf, parentDirectoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	parentID, err := parseFileID(parentDirectoryID)
	if err != nil {
		return nil, err
	}
	created, err := f.createUpload(ctx, parentID, leaf, prepared.md5, prepared.size)
	if err != nil {
		return nil, err
	}
	if created.Data.Reuse {
		if created.Data.FileID == 0 {
			return nil, errors.New("123Pan instant upload returned an empty file ID")
		}
		f.dirCache.FlushDir(parentDir(remote))
		return &Object{
			fs:      f,
			remote:  remote,
			id:      created.Data.FileID,
			parent:  parentID,
			size:    prepared.size,
			md5sum:  strings.ToLower(prepared.md5),
			modTime: src.ModTime(ctx),
		}, nil
	}
	if created.Data.PreuploadID == "" || created.Data.SliceSize <= 0 || len(created.Data.Servers) == 0 {
		return nil, errors.New("123Pan upload creation returned incomplete multipart information")
	}
	if f.renewer != nil {
		f.renewer.Start()
		defer f.renewer.Stop()
	}
	remaining := prepared.size
	for partNumber := 1; remaining > 0; partNumber++ {
		partSize := created.Data.SliceSize
		if remaining < partSize {
			partSize = remaining
		}
		contents := make([]byte, partSize)
		if _, err = io.ReadFull(prepared.reader, contents); err != nil {
			return nil, fmt.Errorf("read 123Pan upload slice %d: %w", partNumber, err)
		}
		if err = f.uploadSlice(ctx, created.Data.Servers[0], created.Data.PreuploadID, partNumber, leaf, contents); err != nil {
			return nil, err
		}
		remaining -= partSize
	}
	fileID, err := f.completeUpload(ctx, created.Data.PreuploadID)
	if err != nil {
		return nil, err
	}
	f.dirCache.FlushDir(parentDir(remote))
	return &Object{
		fs:      f,
		remote:  remote,
		id:      fileID,
		parent:  parentID,
		size:    prepared.size,
		md5sum:  strings.ToLower(prepared.md5),
		modTime: src.ModTime(ctx),
	}, nil
}

// Put uploads a file to its source remote path.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.put(ctx, in, src, src.Remote(), options...)
}

// Update replaces o's content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	updated, err := o.fs.put(ctx, in, src, o.remote, options...)
	if err != nil {
		return err
	}
	*o = *updated
	return nil
}

// Copy copies src to remote using the 123Pan server-side copy API.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	source, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}
	leaf, parentDirectoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	parentID, err := parseFileID(parentDirectoryID)
	if err != nil {
		return nil, err
	}
	fileID, err := f.copy(ctx, source.id, parentID)
	if err != nil {
		return nil, err
	}
	if leaf != path.Base(source.remote) {
		if err = f.rename(ctx, fileID, leaf); err != nil {
			return nil, err
		}
	}
	f.dirCache.FlushDir(parentDir(remote))
	return &Object{
		fs:      f,
		remote:  remote,
		id:      fileID,
		parent:  parentID,
		size:    source.size,
		md5sum:  source.md5sum,
		modTime: source.modTime,
	}, nil
}

// Move moves src to remote using the 123Pan server-side move API.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	source, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	leaf, parentDirectoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	parentID, err := parseFileID(parentDirectoryID)
	if err != nil {
		return nil, err
	}
	if source.parent != parentID {
		if err = f.move(ctx, source.id, parentID); err != nil {
			return nil, err
		}
	}
	if leaf != path.Base(source.remote) {
		if err = f.rename(ctx, source.id, leaf); err != nil {
			return nil, err
		}
	}
	source.fs.dirCache.FlushDir(parentDir(source.remote))
	f.dirCache.FlushDir(parentDir(remote))
	return &Object{
		fs:      f,
		remote:  remote,
		id:      source.id,
		parent:  parentID,
		size:    source.size,
		md5sum:  source.md5sum,
		modTime: source.modTime,
	}, nil
}

// DirMove moves a directory to dstRemote using the 123Pan server-side move API.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	sourceFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}
	sourceDirectoryID, sourceParentDirectoryID, sourceLeaf, destinationParentDirectoryID, destinationLeaf, err := f.dirCache.DirMove(ctx, sourceFs.dirCache, sourceFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	sourceID, err := parseFileID(sourceDirectoryID)
	if err != nil {
		return err
	}
	sourceParentID, err := parseFileID(sourceParentDirectoryID)
	if err != nil {
		return err
	}
	destinationParentID, err := parseFileID(destinationParentDirectoryID)
	if err != nil {
		return err
	}
	if sourceParentID != destinationParentID {
		if err = f.move(ctx, sourceID, destinationParentID); err != nil {
			return err
		}
	}
	if sourceLeaf != destinationLeaf {
		if err = f.rename(ctx, sourceID, destinationLeaf); err != nil {
			return err
		}
	}
	sourceFs.dirCache.FlushDir(srcRemote)
	return nil
}

// About returns 123Pan quota information.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var response api.UserInfoResponse
	if err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodGet,
		Path:   "/api/v1/user/info",
	}, nil, &response); err != nil {
		return nil, fmt.Errorf("get 123Pan usage: %w", err)
	}
	return &fs.Usage{
		Total: fs.NewUsageValue(response.Data.SpacePermanent + response.Data.SpaceTemp),
		Used:  fs.NewUsageValue(response.Data.SpaceUsed),
	}, nil
}

// Shutdown stops background token renewal.
func (f *Fs) Shutdown(context.Context) error {
	if f.renewer != nil {
		f.renewer.Shutdown()
		f.renewer = nil
	}
	return nil
}

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ fs.ParentIDer      = (*Object)(nil)
)

type preparedUpload struct {
	reader io.Reader
	size   int64
	md5    string
	close  func()
}

// prepareUpload produces the repeatable input required by the 123Pan upload
// protocol, which needs the whole-file MD5 before it accepts any bytes.
func (f *Fs) prepareUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (*preparedUpload, error) {
	if in == nil {
		return nil, fmt.Errorf("upload input is nil")
	}
	if src == nil {
		return nil, fmt.Errorf("upload source is nil")
	}

	size := src.Size()
	md5sum, err := src.Hash(ctx, hash.MD5)
	if err != nil {
		md5sum = ""
	}
	md5sum = strings.ToLower(md5sum)

	if md5sum != "" && size >= 0 {
		return &preparedUpload{reader: in, size: size, md5: md5sum, close: func() {}}, nil
	}

	if f.opt.NoBuffer {
		if md5sum != "" {
			return &preparedUpload{reader: in, size: size, md5: md5sum, close: func() {}}, nil
		}
		hasher := md5.New()
		n, err := io.Copy(hasher, in)
		if err != nil {
			return nil, fmt.Errorf("calculate upload MD5: %w", err)
		}
		if size < 0 {
			size = n
		}
		if seeker, ok := in.(io.Seeker); ok {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("rewind no_buffer source: %w", err)
			}
			return &preparedUpload{reader: in, size: size, md5: fmt.Sprintf("%x", hasher.Sum(nil)), close: func() {}}, nil
		}
		object, ok := src.(fs.Object)
		if !ok {
			return nil, fmt.Errorf("no_buffer requires a seekable or reopenable source after calculating MD5")
		}
		reader, err := object.Open(ctx, options...)
		if err != nil {
			return nil, fmt.Errorf("reopen no_buffer source after calculating MD5: %w", err)
		}
		return &preparedUpload{
			reader: reader,
			size:   size,
			md5:    fmt.Sprintf("%x", hasher.Sum(nil)),
			close:  func() { _ = reader.Close() },
		}, nil
	}

	limit := int64(f.opt.HashMemoryLimit)
	if limit <= 0 {
		limit = int64(defaultHashMemoryLimit)
	}
	if size >= 0 && size <= limit {
		contents, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("buffer upload in memory: %w", err)
		}
		if md5sum == "" {
			md5sum = fmt.Sprintf("%x", md5.Sum(contents))
		}
		return &preparedUpload{reader: bytes.NewReader(contents), size: int64(len(contents)), md5: md5sum, close: func() {}}, nil
	}

	file, err := os.CreateTemp("", "rclone-123pan-upload-")
	if err != nil {
		return nil, fmt.Errorf("create upload spool: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	hasher := md5.New()
	n, err := io.Copy(io.MultiWriter(file, hasher), in)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spool upload: %w", err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fmt.Errorf("rewind upload spool: %w", err)
	}
	if md5sum == "" {
		md5sum = fmt.Sprintf("%x", hasher.Sum(nil))
	}
	return &preparedUpload{reader: file, size: n, md5: md5sum, close: cleanup}, nil
}
