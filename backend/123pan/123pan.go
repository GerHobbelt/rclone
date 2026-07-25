// Package _123pan provides an interface to the ordinary 123Pan web API.
package _123pan

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiRootURL             = "https://yun.123pan.com/b/api"
	apiPathPrefix          = "/b/api"
	loginRootURL           = "https://login.123pan.com/api"
	defaultPlatform        = "web"
	webOrigin              = "https://yun.123pan.com"
	webUserAgent           = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) openlist-client"
	rootID                 = "0"
	defaultHashMemoryLimit = 16 * fs.Mebi
	minSleep               = 200 * time.Millisecond
	maxSleep               = 2 * time.Second
	copyPollInterval       = time.Second
	copyPollAttempts       = 120
	s3PartSize             = 16 * fs.Mebi
	s3SinglePutSize        = 5 * fs.Gibi
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
		Description: "123Pan",
		NewFs:       NewFs,
		Config:      Config,
		Options: []fs.Option{{
			Name:      "username",
			Help:      "123Pan account email address or phone number.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:       "password",
			Help:       "123Pan account password.",
			Required:   true,
			IsPassword: true,
		}, {
			Name:     "platform",
			Help:     "Platform header sent with ordinary 123Pan web API requests.",
			Default:  defaultPlatform,
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
	Username        string               `config:"username"`
	Password        string               `config:"password"`
	Platform        string               `config:"platform"`
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
	loginSrv    *rest.Client
	downloadSrv *rest.Client
	dirCache    *dircache.DirCache
	pacer       *fs.Pacer
	authMu      sync.Mutex
	accessToken string
	generation  uint64
	copyDelay   time.Duration
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
	s3Key   string
}

// Config asks for ordinary 123Pan web credentials during initial setup and
// explicit reconnect.
func Config(ctx context.Context, name string, m configmap.Mapper, configIn fs.ConfigIn) (*fs.ConfigOut, error) {
	switch configIn.State {
	case "":
		if username, _ := m.Get("username"); username != "" {
			return fs.ConfigConfirm("replace", false, "config_replace", "Replace the saved 123Pan credentials?")
		}
		return fs.ConfigInput("username", "config_username", "123Pan account email address or phone number")
	case "replace":
		if configIn.Result != "true" {
			return nil, nil
		}
		return fs.ConfigInput("username", "config_username", "123Pan account email address or phone number")
	case "username":
		username := strings.TrimSpace(configIn.Result)
		if username == "" {
			return fs.ConfigError("", "123Pan username cannot be empty")
		}
		m.Set("username", username)
		return fs.ConfigPassword("password", "config_password", "123Pan account password")
	case "password":
		if configIn.Result == "" {
			return fs.ConfigError("", "123Pan password cannot be empty")
		}
		m.Set("password", obscure.MustObscure(configIn.Result))
		opt := new(Options)
		if err := configstruct.Set(m, opt); err != nil {
			return nil, err
		}
		if opt.Platform == "" {
			opt.Platform = defaultPlatform
		}
		backend := &Fs{
			opt:      *opt,
			loginSrv: rest.NewClient(fshttp.NewClient(ctx)).SetRoot(loginRootURL),
		}
		if _, _, err := backend.session(ctx); err != nil {
			return nil, fmt.Errorf("sign in to 123Pan: %w", err)
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
	if strings.TrimSpace(opt.Username) == "" || strings.TrimSpace(opt.Password) == "" {
		return nil, errors.New("123Pan username and password must be configured")
	}
	if opt.Platform == "" {
		opt.Platform = defaultPlatform
	}
	f := &Fs{
		name:        name,
		root:        root,
		opt:         *opt,
		srv:         rest.NewClient(fshttp.NewClient(ctx)).SetRoot(apiRootURL),
		loginSrv:    rest.NewClient(fshttp.NewClient(ctx)).SetRoot(loginRootURL),
		downloadSrv: rest.NewClient(fshttp.NewClient(ctx)),
		pacer:       fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep))),
		copyDelay:   copyPollInterval,
	}
	f.dirCache = dircache.New(root, rootID, f)
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		newRoot, remote := dircache.SplitPath(root)
		tempF := &Fs{
			name:        f.name,
			root:        newRoot,
			opt:         f.opt,
			srv:         f.srv,
			loginSrv:    f.loginSrv,
			downloadSrv: f.downloadSrv,
			pacer:       f.pacer,
			copyDelay:   f.copyDelay,
		}
		tempF.dirCache = dircache.New(newRoot, rootID, tempF)
		if err = tempF.dirCache.FindRoot(ctx, false); err != nil {
			return f, nil
		}
		if _, err = tempF.NewObject(ctx, remote); err != nil {
			if err == fs.ErrorObjectNotFound {
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, tempF)
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

// session returns the current web session, logging in when none has been
// established yet.
func (f *Fs) session(ctx context.Context) (string, uint64, error) {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	if f.accessToken != "" {
		return f.accessToken, f.generation, nil
	}
	return f.loginLocked(ctx)
}

// loginLocked signs in with the configured ordinary 123Pan credentials.
//
// f.authMu must be held by the caller.
func (f *Fs) loginLocked(ctx context.Context) (string, uint64, error) {
	if f.loginSrv == nil {
		return "", 0, errors.New("123Pan login client is not initialized")
	}
	password, err := obscure.Reveal(f.opt.Password)
	if err != nil {
		return "", 0, fmt.Errorf("reveal 123Pan password: %w", err)
	}
	username := strings.TrimSpace(f.opt.Username)
	if username == "" || password == "" {
		return "", 0, errors.New("123Pan username and password must be configured")
	}
	body := &api.LoginRequest{
		Passport: username,
		Password: password,
		Remember: true,
	}
	if strings.Contains(username, "@") {
		body = &api.LoginRequest{
			Mail:     username,
			Password: password,
			Type:     2,
		}
	}
	var response api.LoginResponse
	_, err = f.loginSrv.CallJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/user/sign_in",
		ExtraHeaders: map[string]string{
			"Origin":      webOrigin,
			"Referer":     webOrigin + "/",
			"User-Agent":  webUserAgent,
			"Platform":    defaultPlatform,
			"App-Version": "3",
		},
	}, body, &response)
	if err != nil {
		return "", 0, err
	}
	if response.Code != http.StatusOK || response.Data.Token == "" {
		if response.Message == "" {
			response.Message = "ordinary 123Pan login returned no session token"
		}
		return "", 0, errors.New(response.Message)
	}
	f.accessToken = response.Data.Token
	f.generation++
	return f.accessToken, f.generation, nil
}

// refreshSessionIfCurrent establishes a new session only when generation is
// still the session that received an authentication failure.
func (f *Fs) refreshSessionIfCurrent(ctx context.Context, generation uint64) (string, uint64, error) {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	if f.accessToken != "" && f.generation != generation {
		return f.accessToken, f.generation, nil
	}
	f.accessToken = ""
	return f.loginLocked(ctx)
}

// signPath returns the ordinary web API query parameter used by OpenList's
// 123 driver.
func signPath(requestPath string) (string, string) {
	table := []byte{'a', 'd', 'e', 'f', 'g', 'h', 'l', 'm', 'y', 'i', 'j', 'n', 'o', 'p', 'k', 'q', 'r', 's', 't', 'u', 'b', 'c', 'v', 'w', 's', 'z'}
	random := fmt.Sprintf("%.f", math.Round(1e7*rand.Float64()))
	now := time.Now().In(time.FixedZone("CST", 8*60*60))
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nowText := []byte(now.Format("200601021504"))
	for index := range nowText {
		nowText[index] = table[nowText[index]-'0']
	}
	timeSign := strconv.FormatUint(uint64(crc32.ChecksumIEEE(nowText)), 10)
	data := strings.Join([]string{timestamp, random, requestPath, defaultPlatform, "3", timeSign}, "|")
	dataSign := strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(data))), 10)
	return timeSign, strings.Join([]string{timestamp, random, dataSign}, "-")
}

// authOpts adds the ordinary 123Pan session and signed web-request headers.
func (f *Fs) authOpts(opts *rest.Opts, accessToken string) *rest.Opts {
	requestOpts := *opts
	headers := make(map[string]string, len(opts.ExtraHeaders)+6)
	for key, value := range opts.ExtraHeaders {
		headers[key] = value
	}
	headers["Authorization"] = "Bearer " + accessToken
	headers["Origin"] = webOrigin
	headers["Referer"] = webOrigin + "/"
	headers["User-Agent"] = webUserAgent
	headers["Platform"] = f.opt.Platform
	headers["App-Version"] = "3"
	requestOpts.ExtraHeaders = headers
	parameters := make(url.Values, len(opts.Parameters)+1)
	for key, values := range opts.Parameters {
		parameters[key] = append([]string(nil), values...)
	}
	key, value := signPath(apiPathPrefix + opts.Path)
	parameters.Set(key, value)
	requestOpts.Parameters = parameters
	return &requestOpts
}

// retryAuthenticationFailure establishes a fresh web session at most once for
// one API request.
func (f *Fs) retryAuthenticationFailure(ctx context.Context, generation uint64, refreshed *bool) (bool, error) {
	if *refreshed {
		return false, nil
	}
	if _, _, err := f.refreshSessionIfCurrent(ctx, generation); err != nil {
		return false, err
	}
	*refreshed = true
	return true, nil
}

// callJSON calls an authenticated ordinary API endpoint. A 401 establishes a
// new session and retries the request once.
func (f *Fs) callJSON(ctx context.Context, opts *rest.Opts, request any, response apiResponse) error {
	refreshed := false
	return f.pacer.Call(func() (bool, error) {
		accessToken, generation, err := f.session(ctx)
		if err != nil {
			return false, err
		}
		resp, err := f.srv.CallJSON(ctx, f.authOpts(opts, accessToken), request, response)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusUnauthorized {
				retry, loginErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if loginErr != nil {
					return false, fmt.Errorf("sign in after 123Pan authentication failure: %w", loginErr)
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
				retry, loginErr := f.retryAuthenticationFailure(ctx, generation, &refreshed)
				if loginErr != nil {
					return false, fmt.Errorf("sign in after 123Pan authentication failure: %w", loginErr)
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
	for page := 1; ; page++ {
		if page >= 100000 {
			return nil, errors.New("too many 123Pan file-list pages")
		}
		var response api.FileListResponse
		err := f.callJSON(ctx, &rest.Opts{
			Method: http.MethodGet,
			Path:   "/file/list/new",
			Parameters: url.Values{
				"driveId":              {"0"},
				"limit":                {"100"},
				"next":                 {"0"},
				"orderBy":              {"file_id"},
				"orderDirection":       {"desc"},
				"parentFileId":         {strconv.FormatInt(parentID, 10)},
				"trashed":              {"false"},
				"SearchData":           {""},
				"Page":                 {strconv.Itoa(page)},
				"OnlyLookAbnormalFile": {"0"},
				"event":                {"homeListFile"},
				"operateType":          {"4"},
				"inDirectSpace":        {"false"},
			},
		}, nil, &response)
		if err != nil {
			return nil, fmt.Errorf("list 123Pan files: %w", err)
		}
		files = append(files, response.Data.InfoList...)
		if len(response.Data.InfoList) == 0 || response.Data.Next == "-1" {
			return files, nil
		}
	}
}

func (f *Fs) findFile(ctx context.Context, parentID int64, leaf string) (*api.File, error) {
	files, err := f.listFiles(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for index := range files {
		if f.opt.Enc.ToStandardName(files[index].FileName) == leaf {
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
	var response api.UploadResponse
	err = f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/file/upload_request",
	}, &api.UploadRequest{
		DriveID:      0,
		ETag:         "",
		FileName:     f.opt.Enc.FromStandardName(leaf),
		ParentFileID: parentID,
		Size:         0,
		Type:         1,
	}, &response)
	if err != nil {
		if id, found, findErr := f.FindLeaf(ctx, directoryID, leaf); findErr == nil && found {
			return id, nil
		}
		return "", fmt.Errorf("create 123Pan directory %q: %w", leaf, err)
	}
	if response.Data.FileID == 0 {
		return "", errors.New("123Pan mkdir returned an empty directory ID")
	}
	return strconv.FormatInt(response.Data.FileID, 10), nil
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
	for index := range files {
		file := &files[index]
		file.ParentFileID = parentID
		remote := path.Join(dir, f.opt.Enc.ToStandardName(file.FileName))
		if file.Type == 1 {
			id := strconv.FormatInt(file.FileID, 10)
			entries = append(entries, fs.NewDir(remote, parseFileTime(file.UpdateAt)).SetID(id).SetParentID(directoryID))
			f.dirCache.Put(remote, id)
			continue
		}
		entries = append(entries, f.newObject(remote, file))
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
	file.ParentFileID = parentID
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
		s3Key:   file.S3KeyFlag,
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
		Path:   "/file/trash",
	}, &api.TrashRequest{
		DriveID:   0,
		Operation: true,
		FileTrashInfoList: []api.FileID{{
			FileID: id,
		}},
	}, response)
	if err != nil {
		return fmt.Errorf("move 123Pan file to recycle bin: %w", err)
	}
	return nil
}

func (f *Fs) rename(ctx context.Context, id int64, name string) error {
	response := new(api.Response)
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/file/rename",
	}, &api.RenameRequest{
		DriveID:  0,
		FileID:   id,
		FileName: f.opt.Enc.FromStandardName(name),
	}, response)
	if err != nil {
		return fmt.Errorf("rename 123Pan file: %w", err)
	}
	return nil
}

func (f *Fs) move(ctx context.Context, id, parentID int64) error {
	response := new(api.Response)
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/file/mod_pid",
	}, &api.MoveRequest{
		FileIDList:   []api.FileID{{FileID: id}},
		ParentFileID: parentID,
	}, response)
	if err != nil {
		return fmt.Errorf("move 123Pan file: %w", err)
	}
	return nil
}

func (f *Fs) copy(ctx context.Context, source *Object, parentID int64) error {
	var response api.CopyStartResponse
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/restful/goapi/v1/file/copy/async",
	}, &api.CopyRequest{
		FileList: []api.CopyFile{{
			FileID:       source.id,
			Size:         source.size,
			ETag:         source.md5sum,
			Type:         0,
			ParentFileID: source.parent,
			FileName:     f.opt.Enc.FromStandardName(path.Base(source.remote)),
			DriveID:      0,
		}},
		TargetFileID: parentID,
	}, &response)
	if err != nil {
		return fmt.Errorf("start 123Pan copy task: %w", err)
	}
	if response.Data.TaskID == 0 {
		return errors.New("123Pan copy returned an empty task ID")
	}
	for attempt := 0; attempt < copyPollAttempts; attempt++ {
		var task api.CopyTaskResponse
		if err = f.callJSON(ctx, &rest.Opts{
			Method: http.MethodGet,
			Path:   "/restful/goapi/v1/file/copy/task",
			Parameters: url.Values{
				"taskId": {strconv.FormatInt(response.Data.TaskID, 10)},
			},
		}, nil, &task); err != nil {
			return fmt.Errorf("check 123Pan copy task: %w", err)
		}
		if task.Data.ErrorCode != 0 {
			if task.Data.Reason == "" {
				task.Data.Reason = "123Pan copy task failed"
			}
			return fmt.Errorf("%s (error code %d)", task.Data.Reason, task.Data.ErrorCode)
		}
		if task.Data.Status == 2 {
			return nil
		}
		if task.Data.Status != 1 {
			return fmt.Errorf("123Pan copy task returned unexpected status %d", task.Data.Status)
		}
		delay := f.copyDelay
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return errors.New("123Pan copy task did not finish before the polling deadline")
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

// SetModTime reports that the ordinary API does not support setting modtime.
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
		Method: http.MethodPost,
		Path:   "/file/download_info",
	}, &api.DownloadInfoRequest{
		DriveID:   0,
		ETag:      o.md5sum,
		FileID:    o.id,
		FileName:  o.fs.opt.Enc.FromStandardName(path.Base(o.remote)),
		S3KeyFlag: o.s3Key,
		Size:      o.size,
		Type:      0,
	}, &info); err != nil {
		return nil, fmt.Errorf("get 123Pan download URL: %w", err)
	}
	if info.Data.DownloadURL == "" {
		return nil, errors.New("123Pan returned an empty download URL")
	}
	downloadURL := info.Data.DownloadURL
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("parse 123Pan download URL: %w", err)
	}
	if encodedURL := parsedURL.Query().Get("params"); encodedURL != "" {
		decodedURL, decodeErr := base64.StdEncoding.DecodeString(encodedURL)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode 123Pan download URL: %w", decodeErr)
		}
		if _, parseErr := url.Parse(string(decodedURL)); parseErr != nil {
			return nil, fmt.Errorf("parse decoded 123Pan download URL: %w", parseErr)
		}
		downloadURL = string(decodedURL)
	}
	var response *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		var callErr error
		response, callErr = o.fs.downloadSrv.Call(ctx, &rest.Opts{
			Method:  http.MethodGet,
			RootURL: downloadURL,
			ExtraHeaders: map[string]string{
				"Referer": fmt.Sprintf("%s://%s/", parsedURL.Scheme, parsedURL.Host),
			},
			Options: options,
		})
		if callErr != nil && fserrors.ShouldRetry(callErr) {
			return true, callErr
		}
		return false, callErr
	})
	if err != nil {
		return nil, fmt.Errorf("open 123Pan file: %w", err)
	}
	return response.Body, nil
}

func (f *Fs) createUpload(ctx context.Context, parentID int64, leaf, md5sum string, size int64) (*api.UploadResponse, error) {
	var response api.UploadResponse
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/file/upload_request",
	}, &api.UploadRequest{
		DriveID:      0,
		Duplicate:    2,
		ETag:         strings.ToLower(md5sum),
		FileName:     f.opt.Enc.FromStandardName(leaf),
		ParentFileID: parentID,
		Size:         size,
		Type:         0,
	}, &response)
	if err != nil {
		return nil, fmt.Errorf("create 123Pan upload: %w", err)
	}
	return &response, nil
}

func (f *Fs) uploadWithTemporaryS3(ctx context.Context, upload *api.UploadResponse, in io.Reader, size int64) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("123pan"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(upload.Data.AccessKeyID, upload.Data.SecretAccessKey, upload.Data.SessionToken)),
		awsconfig.WithHTTPClient(fshttp.NewClient(ctx)),
	)
	if err != nil {
		return fmt.Errorf("configure temporary 123Pan S3 upload: %w", err)
	}
	if upload.Data.EndPoint != "" {
		awsCfg.BaseEndpoint = aws.String(upload.Data.EndPoint)
	}
	awsCfg.RetryMaxAttempts = fs.GetConfig(ctx).LowLevelRetries
	client := awss3.NewFromConfig(awsCfg, func(options *awss3.Options) {
		options.UsePathStyle = true
	})
	bucket := aws.String(upload.Data.Bucket)
	key := aws.String(upload.Data.Key)
	if size <= int64(s3SinglePutSize) {
		_, err = client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket:        bucket,
			Key:           key,
			Body:          in,
			ContentLength: aws.Int64(size),
		})
		if err != nil {
			return fmt.Errorf("upload 123Pan temporary S3 object: %w", err)
		}
		return nil
	}

	started, err := client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		return fmt.Errorf("start 123Pan temporary S3 multipart upload: %w", err)
	}
	if started.UploadId == nil || *started.UploadId == "" {
		return errors.New("123Pan temporary S3 upload returned an empty upload ID")
	}
	completed := false
	defer func() {
		if !completed {
			_, _ = client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
				Bucket:   bucket,
				Key:      key,
				UploadId: started.UploadId,
			})
		}
	}()

	parts := make([]awss3types.CompletedPart, 0, (size+int64(s3PartSize)-1)/int64(s3PartSize))
	for number, remaining := int32(1), size; remaining > 0; number++ {
		partSize := min(remaining, int64(s3PartSize))
		result, partErr := client.UploadPart(ctx, &awss3.UploadPartInput{
			Bucket:        bucket,
			Key:           key,
			UploadId:      started.UploadId,
			PartNumber:    aws.Int32(number),
			Body:          io.LimitReader(in, partSize),
			ContentLength: aws.Int64(partSize),
		})
		if partErr != nil {
			return fmt.Errorf("upload 123Pan temporary S3 part %d: %w", number, partErr)
		}
		if result.ETag == nil || *result.ETag == "" {
			return fmt.Errorf("123Pan temporary S3 part %d returned no ETag", number)
		}
		parts = append(parts, awss3types.CompletedPart{
			ETag:       result.ETag,
			PartNumber: aws.Int32(number),
		})
		remaining -= partSize
	}
	_, err = client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   bucket,
		Key:      key,
		UploadId: started.UploadId,
		MultipartUpload: &awss3types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("complete 123Pan temporary S3 multipart upload: %w", err)
	}
	completed = true
	return nil
}

func (f *Fs) preSignedURLs(ctx context.Context, upload *api.UploadResponse, start, end int, multipart bool) (map[string]string, error) {
	endpoint := "/file/s3_upload_object/auth"
	if multipart {
		endpoint = "/file/s3_repare_upload_parts_batch"
	}
	var response api.S3PreSignedURLsResponse
	err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   endpoint,
	}, &api.S3URLsRequest{
		StorageNode:     upload.Data.StorageNode,
		Bucket:          upload.Data.Bucket,
		Key:             upload.Data.Key,
		PartNumberEnd:   end,
		PartNumberStart: start,
		UploadID:        upload.Data.UploadID,
	}, &response)
	if err != nil {
		return nil, fmt.Errorf("get 123Pan temporary S3 URLs: %w", err)
	}
	if len(response.Data.PreSignedURLs) == 0 {
		return nil, errors.New("123Pan returned no temporary S3 URLs")
	}
	return response.Data.PreSignedURLs, nil
}

func (f *Fs) uploadPreSignedPart(ctx context.Context, uploadURL string, in io.Reader, size int64) error {
	// A presigned request consumes a potentially non-seekable no_buffer source;
	// retrying it would upload an incomplete part.
	return f.pacer.CallNoRetry(func() (bool, error) {
		response, err := f.downloadSrv.Call(ctx, &rest.Opts{
			Method:        http.MethodPut,
			RootURL:       uploadURL,
			Body:          in,
			ContentLength: &size,
			NoResponse:    true,
		})
		if err != nil && fserrors.ShouldRetry(err) {
			return true, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return false, err
	})
}

func (f *Fs) uploadWithPreSignedURLs(ctx context.Context, upload *api.UploadResponse, in io.Reader, size int64) error {
	partCount := int((size + int64(s3PartSize) - 1) / int64(s3PartSize))
	for start := 1; start <= partCount; {
		end := start + 1
		multipart := partCount > 1
		if multipart {
			end = min(start+10, partCount+1)
		}
		urls, err := f.preSignedURLs(ctx, upload, start, end, multipart)
		if err != nil {
			return err
		}
		for number := start; number < end; number++ {
			partSize := int64(s3PartSize)
			if remaining := size - int64(number-1)*int64(s3PartSize); remaining < partSize {
				partSize = remaining
			}
			uploadURL := urls[strconv.Itoa(number)]
			if uploadURL == "" {
				return fmt.Errorf("123Pan returned no temporary S3 URL for part %d", number)
			}
			if err = f.uploadPreSignedPart(ctx, uploadURL, io.LimitReader(in, partSize), partSize); err != nil {
				return fmt.Errorf("upload 123Pan temporary S3 part %d: %w", number, err)
			}
		}
		start = end
	}
	response := new(api.Response)
	if err := f.callJSON(ctx, &rest.Opts{
		Method: http.MethodPost,
		Path:   "/file/upload_complete/v2",
	}, &api.S3UploadCompleteRequest{
		StorageNode: upload.Data.StorageNode,
		Bucket:      upload.Data.Bucket,
		FileID:      upload.Data.FileID,
		FileSize:    size,
		IsMultipart: partCount > 1,
		Key:         upload.Data.Key,
		UploadID:    upload.Data.UploadID,
	}, response); err != nil {
		return fmt.Errorf("complete 123Pan temporary S3 upload: %w", err)
	}
	return nil
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
	if created.Data.Reuse || created.Data.Key == "" {
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
	if created.Data.FileID == 0 {
		return nil, errors.New("123Pan upload creation returned an empty file ID")
	}
	if created.Data.AccessKeyID != "" && created.Data.SecretAccessKey != "" && created.Data.SessionToken != "" {
		err = f.uploadWithTemporaryS3(ctx, created, prepared.reader, prepared.size)
		if err == nil {
			response := new(api.Response)
			err = f.callJSON(ctx, &rest.Opts{
				Method: http.MethodPost,
				Path:   "/file/upload_complete",
			}, &api.UploadCompleteRequest{FileID: created.Data.FileID}, response)
		}
	} else {
		err = f.uploadWithPreSignedURLs(ctx, created, prepared.reader, prepared.size)
	}
	if err != nil {
		return nil, err
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
	if err = f.copy(ctx, source, parentID); err != nil {
		return nil, err
	}
	copyRemote := path.Join(parentDir(remote), path.Base(source.remote))
	copied, err := f.NewObject(ctx, copyRemote)
	if err != nil {
		return nil, fmt.Errorf("find copied 123Pan object: %w", err)
	}
	result := copied.(*Object)
	if leaf != path.Base(source.remote) {
		if err = f.rename(ctx, result.id, leaf); err != nil {
			return nil, err
		}
		result.remote = remote
	}
	f.dirCache.FlushDir(parentDir(remote))
	return result, nil
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
		s3Key:   source.s3Key,
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
		Path:   "/user/info",
	}, nil, &response); err != nil {
		return nil, fmt.Errorf("get 123Pan usage: %w", err)
	}
	return &fs.Usage{
		Total: fs.NewUsageValue(response.Data.SpacePermanent + response.Data.SpaceTemp),
		Used:  fs.NewUsageValue(response.Data.SpaceUsed),
	}, nil
}

// Shutdown releases backend resources.
func (f *Fs) Shutdown(context.Context) error {
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
