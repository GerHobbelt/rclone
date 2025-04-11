package vault

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"

	"github.com/rclone/rclone/backend/vault/api"
	"github.com/rclone/rclone/fstest/fstests"
)

const (
	testEndpoint = "http://localhost:8000/api"
	testUsername = "admin"
	testPassword = "admin"
)

// TestIntegration runs integration tests against the remote. This is a set of
// test supplied by rclone, of which we still fail a lot.
//
//	$ VAULT_TEST_REMOTE_NAME=v: go test -v ./backend/vault/...
func TestIntegration(t *testing.T) {
	// t.Skip("skipping integration tests temporarily")
	var remoteName string
	if v := os.Getenv("VAULT_TEST_REMOTE_NAME"); v != "" {
		remoteName = v
	} else {
		t.Skip("VAULT_TEST_REMOTE_NAME env not set, skipping")
	}
	// TODO(martin): collection (top level dirs) cannot be deleted, but that
	// leads to failing tests; fix this.
	fstests.Run(t, &fstests.Opt{
		RemoteName:               remoteName,
		NilObject:                (*Object)(nil),
		SkipBadWindowsCharacters: true,
		SkipDirectoryCheckWrap:   true,
		SkipFsCheckWrap:          true,
		SkipInvalidUTF8:          true,
	})
}

// randomName returns a name that can be used for files, directories and
// collections.
func randomName(tag string) string {
	return fmt.Sprintf("%s-%024d", tag, rand.Int63())
}

// mustLogin returns an authenticated client.
func mustLogin(t *testing.T) *api.API {
	api := api.New(testEndpoint, testUsername, testPassword)
	if err := api.Login(); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	return api
}

// mustCollection creates and returns a collection with a given name.
func mustCollection(t *testing.T, api *api.API, name string) *api.Collection {
	ctx := context.Background()
	err := api.CreateCollection(ctx, name)
	if err != nil {
		t.Fatalf("failed to create collection: %v", err)
	}
	t.Logf("created collection %v", name)
	vs := url.Values{}
	vs.Set("name", name)
	result, err := api.FindCollections(vs)
	if err != nil {
		t.Fatalf("failed to query collections: %v, %v", result, err)
	}
	if len(result) != 1 {
		t.Fatalf("expected a single result, got %v", len(result))
	}
	return result[0]
}

func mustTreeNodeForCollection(t *testing.T, api *api.API, c *api.Collection) *api.TreeNode {
	vs := url.Values{}
	vs.Set("id", fmt.Sprintf("%d", c.TreeNodeIdentifier()))
	t.Logf("finding treenode: %v", c.TreeNodeIdentifier())
	ts, err := api.FindTreeNodes(vs)
	if err != nil {
		t.Fatalf("failed to get treenode: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected single result, got %v", len(ts))
	}
	return ts[0]
}

// TestCreateCollection tests collection creation.
func TestCreateCollection(t *testing.T) {
	var (
		api  = mustLogin(t)
		name = randomName("test-collection")
	)
	_ = mustCollection(t, api, name)
	t.Logf("created collection: %v", name)
}

func TestCreateFolder(t *testing.T) {
	var (
		ctx            = context.Background()
		api            = mustLogin(t)
		collectionName = randomName("test-collection")
		collection     = mustCollection(t, api, collectionName)
		treeNode       = mustTreeNodeForCollection(t, api, collection)
		folderName     = randomName("test-folder")
	)
	err := api.CreateFolder(ctx, treeNode, folderName)
	if err != nil {
		t.Fatalf("failed to create folder: %v", err)
	}
	t.Logf("created collection and folder: %v/%v", collectionName, folderName)
}

func TestRegisterDeposit(t *testing.T) {
	t.Skip("obsolete")
}

func TestGetFlowIdentifier(t *testing.T) {
	fs := &Fs{root: "/"}
	src := &Object{
		fs:     fs,
		remote: "r",
		treeNode: &api.TreeNode{
			Comment: "dummy treenode",
		},
	}
	v, err := fs.getFlowIdentifier(src)
	if err != nil {
		t.Errorf("failed to get flow identifier: %v", err)
	}
	if want := "rclone-vault-flow-84a91929c5ae7f4e7a4c33c1b98454f2"; v != want {
		t.Errorf("getFlowIdentifier: got %v, want %v", v, want)
	}
}

func TestGetTotalChunks(t *testing.T) {
	var cases = []struct {
		about      string
		objectSize int
		chunkSize  int64
		expected   int
	}{
		{"zero size and chunk, we need at least one chunk for transmission", 0, 0, 1},
		{"zero size, we need at least one chunk for transmission, even if the file is size zero", 0, 9, 1},
		{"single chunk", 1, 1, 1},
		{"many chunks", 9, 1, 9},
		{"small object", 8, 9, 1},
	}
	for _, c := range cases {
		result := getFlowTotalChunks(c.objectSize, c.chunkSize)
		if result != c.expected {
			t.Errorf("getFlowTotalChunks [%s]: got %v, want %v", c.about, result, c.expected)
		}
	}
}

func TestDeposit(t *testing.T)      {}
func TestFileRename(t *testing.T)   {}
func TestFileMove(t *testing.T)     {}
func TestFolderRename(t *testing.T) {}
func TestFolderMove(t *testing.T)   {}

// TODO:
//
// [ ] register deposit
// [ ] upload file
// [ ] rename file
// [ ] move file
// [ ] move folder
// [ ] delete folder

// FROM: VAULT_TEST_REMOTE_NAME=vo: go test -v ./backend/vault/...
//
// --- FAIL: TestIntegration (280.81s)
//     --- SKIP: TestIntegration/FsCheckWrap (0.00s)
//     --- SKIP: TestIntegration/FsCommand (0.00s)
//     --- PASS: TestIntegration/FsRmdirNotFound (0.29s)
//     --- PASS: TestIntegration/FsString (0.00s)
//     --- PASS: TestIntegration/FsName (0.00s)
//     --- PASS: TestIntegration/FsRoot (0.00s)
//     --- PASS: TestIntegration/FsRmdirEmpty (0.26s)
//     --- FAIL: TestIntegration/FsMkdir (278.89s)
//         --- PASS: TestIntegration/FsMkdir/FsMkdirRmdirSubdir (4.74s)
//         --- PASS: TestIntegration/FsMkdir/FsListEmpty (0.22s)
//         --- PASS: TestIntegration/FsMkdir/FsListDirEmpty (0.25s)
//         --- SKIP: TestIntegration/FsMkdir/FsListRDirEmpty (0.00s)
//         --- PASS: TestIntegration/FsMkdir/FsListDirNotFound (0.24s)
//         --- SKIP: TestIntegration/FsMkdir/FsListRDirNotFound (0.00s)
//         --- FAIL: TestIntegration/FsMkdir/FsEncoding (261.58s)
//             --- PASS: TestIntegration/FsMkdir/FsEncoding/control_chars (2.80s)
//             --- PASS: TestIntegration/FsMkdir/FsEncoding/dot (2.40s)
//             --- PASS: TestIntegration/FsMkdir/FsEncoding/dot_dot (2.46s)
//             --- PASS: TestIntegration/FsMkdir/FsEncoding/punctuation (2.20s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_space (6.12s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_tilde (19.44s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_CR (19.25s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_LF (19.22s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_HT (19.50s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_VT (19.39s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/leading_dot (19.20s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_space (6.19s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_CR (20.49s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_LF (20.59s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_HT (20.55s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_VT (20.30s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/trailing_dot (20.44s)
//             --- SKIP: TestIntegration/FsMkdir/FsEncoding/invalid_UTF-8 (0.00s)
//             --- FAIL: TestIntegration/FsMkdir/FsEncoding/URL_encoding (20.82s)
//         --- PASS: TestIntegration/FsMkdir/FsNewObjectNotFound (0.49s)
//         --- PASS: TestIntegration/FsMkdir/FsPutError (0.27s)
//         --- PASS: TestIntegration/FsMkdir/FsPutZeroLength (0.53s)
//         --- SKIP: TestIntegration/FsMkdir/FsOpenWriterAt (0.00s)
//         --- SKIP: TestIntegration/FsMkdir/FsOpenChunkWriter (0.00s)
//         --- SKIP: TestIntegration/FsMkdir/FsChangeNotify (0.00s)
//         --- FAIL: TestIntegration/FsMkdir/FsPutFiles (7.22s)
//         --- SKIP: TestIntegration/FsMkdir/FsPutChunked (0.00s)
//         --- SKIP: TestIntegration/FsMkdir/FsCopyChunked (0.00s)
//         --- FAIL: TestIntegration/FsMkdir/FsUploadUnknownSize (1.19s)
//             --- FAIL: TestIntegration/FsMkdir/FsUploadUnknownSize/FsPutUnknownSize (0.29s)
//             --- PASS: TestIntegration/FsMkdir/FsUploadUnknownSize/FsUpdateUnknownSize (0.90s)
//         --- PASS: TestIntegration/FsMkdir/FsRootCollapse (0.82s)
//         --- SKIP: TestIntegration/FsMkdir/FsDirSetModTime (0.00s)
//         --- SKIP: TestIntegration/FsMkdir/FsMkdirMetadata (0.00s)
//         --- SKIP: TestIntegration/FsMkdir/FsDirectory (0.00s)
//     --- PASS: TestIntegration/FsShutdown (0.12s)
