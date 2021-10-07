package operations_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/artpar/rclone/fs"
	"github.com/artpar/rclone/fs/accounting"
	"github.com/artpar/rclone/fs/hash"
	"github.com/artpar/rclone/fs/operations"
	"github.com/artpar/rclone/fstest"
	"github.com/artpar/rclone/lib/readers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCheck(t *testing.T, checkFunction func(ctx context.Context, opt *operations.CheckOpt) error) {
	r := fstest.NewRun(t)
	defer r.Finalise()
	ctx := context.Background()
	ci := fs.GetConfig(ctx)

	addBuffers := func(opt *operations.CheckOpt) {
		opt.Combined = new(bytes.Buffer)
		opt.MissingOnSrc = new(bytes.Buffer)
		opt.MissingOnDst = new(bytes.Buffer)
		opt.Match = new(bytes.Buffer)
		opt.Differ = new(bytes.Buffer)
		opt.Error = new(bytes.Buffer)
	}

	sortLines := func(in string) []string {
		if in == "" {
			return []string{}
		}
		lines := strings.Split(in, "\n")
		sort.Strings(lines)
		return lines
	}

	checkBuffer := func(name string, want map[string]string, out io.Writer) {
		expected := want[name]
		buf, ok := out.(*bytes.Buffer)
		require.True(t, ok)
		assert.Equal(t, sortLines(expected), sortLines(buf.String()), name)
	}

	checkBuffers := func(opt *operations.CheckOpt, want map[string]string) {
		checkBuffer("combined", want, opt.Combined)
		checkBuffer("missingonsrc", want, opt.MissingOnSrc)
		checkBuffer("missingondst", want, opt.MissingOnDst)
		checkBuffer("match", want, opt.Match)
		checkBuffer("differ", want, opt.Differ)
		checkBuffer("error", want, opt.Error)
	}

	check := func(i int, wantErrors int64, wantChecks int64, oneway bool, wantOutput map[string]string) {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			accounting.GlobalStats().ResetCounters()
			var buf bytes.Buffer
			log.SetOutput(&buf)
			defer func() {
				log.SetOutput(os.Stderr)
			}()
			opt := operations.CheckOpt{
				Fdst:   r.Fremote,
				Fsrc:   r.Flocal,
				OneWay: oneway,
			}
			addBuffers(&opt)
			err := checkFunction(ctx, &opt)
			gotErrors := accounting.GlobalStats().GetErrors()
			gotChecks := accounting.GlobalStats().GetChecks()
			if wantErrors == 0 && err != nil {
				t.Errorf("%d: Got error when not expecting one: %v", i, err)
			}
			if wantErrors != 0 && err == nil {
				t.Errorf("%d: No error when expecting one", i)
			}
			if wantErrors != gotErrors {
				t.Errorf("%d: Expecting %d errors but got %d", i, wantErrors, gotErrors)
			}
			if gotChecks > 0 && !strings.Contains(buf.String(), "matching files") {
				t.Errorf("%d: Total files matching line missing", i)
			}
			if wantChecks != gotChecks {
				t.Errorf("%d: Expecting %d total matching files but got %d", i, wantChecks, gotChecks)
			}
			checkBuffers(&opt, wantOutput)
		})
	}

	file1 := r.WriteBoth(ctx, "rutabaga", "is tasty", t3)
	fstest.CheckItems(t, r.Fremote, file1)
	fstest.CheckItems(t, r.Flocal, file1)
	check(1, 0, 1, false, map[string]string{
		"combined":     "= rutabaga\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "rutabaga\n",
		"differ":       "",
		"error":        "",
	})

	file2 := r.WriteFile("potato2", "------------------------------------------------------------", t1)
	fstest.CheckItems(t, r.Flocal, file1, file2)
	check(2, 1, 1, false, map[string]string{
		"combined":     "+ potato2\n= rutabaga\n",
		"missingonsrc": "",
		"missingondst": "potato2\n",
		"match":        "rutabaga\n",
		"differ":       "",
		"error":        "",
	})

	file3 := r.WriteObject(ctx, "empty space", "-", t2)
	fstest.CheckItems(t, r.Fremote, file1, file3)
	check(3, 2, 1, false, map[string]string{
		"combined":     "- empty space\n+ potato2\n= rutabaga\n",
		"missingonsrc": "empty space\n",
		"missingondst": "potato2\n",
		"match":        "rutabaga\n",
		"differ":       "",
		"error":        "",
	})

	file2r := file2
	if ci.SizeOnly {
		file2r = r.WriteObject(ctx, "potato2", "--Some-Differences-But-Size-Only-Is-Enabled-----------------", t1)
	} else {
		r.WriteObject(ctx, "potato2", "------------------------------------------------------------", t1)
	}
	fstest.CheckItems(t, r.Fremote, file1, file2r, file3)
	check(4, 1, 2, false, map[string]string{
		"combined":     "- empty space\n= potato2\n= rutabaga\n",
		"missingonsrc": "empty space\n",
		"missingondst": "",
		"match":        "rutabaga\npotato2\n",
		"differ":       "",
		"error":        "",
	})

	file3r := file3
	file3l := r.WriteFile("empty space", "DIFFER", t2)
	fstest.CheckItems(t, r.Flocal, file1, file2, file3l)
	check(5, 1, 3, false, map[string]string{
		"combined":     "* empty space\n= potato2\n= rutabaga\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "potato2\nrutabaga\n",
		"differ":       "empty space\n",
		"error":        "",
	})

	file4 := r.WriteObject(ctx, "remotepotato", "------------------------------------------------------------", t1)
	fstest.CheckItems(t, r.Fremote, file1, file2r, file3r, file4)
	check(6, 2, 3, false, map[string]string{
		"combined":     "* empty space\n= potato2\n= rutabaga\n- remotepotato\n",
		"missingonsrc": "remotepotato\n",
		"missingondst": "",
		"match":        "potato2\nrutabaga\n",
		"differ":       "empty space\n",
		"error":        "",
	})
	check(7, 1, 3, true, map[string]string{
		"combined":     "* empty space\n= potato2\n= rutabaga\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "potato2\nrutabaga\n",
		"differ":       "empty space\n",
		"error":        "",
	})
}

func TestCheck(t *testing.T) {
	testCheck(t, operations.Check)
}

func TestCheckFsError(t *testing.T) {
	ctx := context.Background()
	dstFs, err := fs.NewFs(ctx, "non-existent")
	if err != nil {
		t.Fatal(err)
	}
	srcFs, err := fs.NewFs(ctx, "non-existent")
	if err != nil {
		t.Fatal(err)
	}
	opt := operations.CheckOpt{
		Fdst:   dstFs,
		Fsrc:   srcFs,
		OneWay: false,
	}
	err = operations.Check(ctx, &opt)
	require.Error(t, err)
}

func TestCheckDownload(t *testing.T) {
	testCheck(t, operations.CheckDownload)
}

func TestCheckSizeOnly(t *testing.T) {
	ctx := context.Background()
	ci := fs.GetConfig(ctx)
	ci.SizeOnly = true
	defer func() { ci.SizeOnly = false }()
	TestCheck(t)
}

func TestCheckEqualReaders(t *testing.T) {
	b65a := make([]byte, 65*1024)
	b65b := make([]byte, 65*1024)
	b65b[len(b65b)-1] = 1
	b66 := make([]byte, 66*1024)

	differ, err := operations.CheckEqualReaders(bytes.NewBuffer(b65a), bytes.NewBuffer(b65a))
	assert.NoError(t, err)
	assert.Equal(t, differ, false)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b65a), bytes.NewBuffer(b65b))
	assert.NoError(t, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b65a), bytes.NewBuffer(b66))
	assert.NoError(t, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b66), bytes.NewBuffer(b65a))
	assert.NoError(t, err)
	assert.Equal(t, differ, true)

	myErr := errors.New("sentinel")
	wrap := func(b []byte) io.Reader {
		r := bytes.NewBuffer(b)
		e := readers.ErrorReader{Err: myErr}
		return io.MultiReader(r, e)
	}

	differ, err = operations.CheckEqualReaders(wrap(b65a), bytes.NewBuffer(b65a))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(wrap(b65a), bytes.NewBuffer(b65b))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(wrap(b65a), bytes.NewBuffer(b66))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(wrap(b66), bytes.NewBuffer(b65a))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b65a), wrap(b65a))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b65a), wrap(b65b))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b65a), wrap(b66))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)

	differ, err = operations.CheckEqualReaders(bytes.NewBuffer(b66), wrap(b65a))
	assert.Equal(t, myErr, err)
	assert.Equal(t, differ, true)
}

func TestParseSumFile(t *testing.T) {
	r := fstest.NewRun(t)
	defer r.Finalise()
	ctx := context.Background()

	const sumFile = "test.sum"

	samples := []struct {
		hash, sep, name string
		ok              bool
	}{
		{"1", "  ", "file1", true},
		{"2", " *", "file2", true},
		{"3", "  ", " file3 ", true},
		{"4", "  ", "\tfile3\t", true},
		{"5", " ", "file5", false},
		{"6", "\t", "file6", false},
		{"7", " \t", " file7 ", false},
		{"", "  ", "file8", false},
		{"", "", "file9", false},
	}

	for _, eol := range []string{"\n", "\r\n"} {
		data := &bytes.Buffer{}
		wantNum := 0
		for _, s := range samples {
			_, _ = data.WriteString(s.hash + s.sep + s.name + eol)
			if s.ok {
				wantNum++
			}
		}

		_ = r.WriteObject(ctx, sumFile, data.String(), t1)
		file, err := r.Fremote.NewObject(ctx, sumFile)
		assert.NoError(t, err)
		sums, err := operations.ParseSumFile(ctx, file)
		assert.NoError(t, err)

		assert.Equal(t, wantNum, len(sums))
		for _, s := range samples {
			if s.ok {
				assert.Equal(t, s.hash, sums[s.name])
			}
		}
	}
}

func testCheckSum(t *testing.T, download bool) {
	const dataDir = "data"
	const sumFile = "test.sum"

	hashType := hash.MD5
	const (
		testString1 = "Hello, World!"
		testDigest1 = "65a8e27d8879283831b664bd8b7f0ad4"
		testString2 = "I am the walrus"
		testDigest2 = "87396e030ef3f5b35bbf85c0a09a4fb3"
	)

	type wantType map[string]string

	ctx := context.Background()
	r := fstest.NewRun(t)
	defer r.Finalise()

	subRemote := r.FremoteName
	if !strings.HasSuffix(subRemote, ":") {
		subRemote += "/"
	}
	subRemote += dataDir
	dataFs, err := fs.NewFs(ctx, subRemote)
	require.NoError(t, err)

	if !download && !dataFs.Hashes().Contains(hashType) {
		t.Skipf("%s lacks %s, skipping", dataFs, hashType)
	}

	makeFile := func(name, content string) fstest.Item {
		remote := dataDir + "/" + name
		return r.WriteObject(ctx, remote, content, t1)
	}

	makeSums := func(sums operations.HashSums) fstest.Item {
		files := make([]string, 0, len(sums))
		for name := range sums {
			files = append(files, name)
		}
		sort.Strings(files)
		buf := &bytes.Buffer{}
		for _, name := range files {
			_, _ = fmt.Fprintf(buf, "%s  %s\n", sums[name], name)
		}
		return r.WriteObject(ctx, sumFile, buf.String(), t1)
	}

	sortLines := func(in string) []string {
		if in == "" {
			return []string{}
		}
		lines := strings.Split(in, "\n")
		sort.Strings(lines)
		return lines
	}

	checkResult := func(runNo int, want wantType, name string, out io.Writer) {
		expected := want[name]
		buf, ok := out.(*bytes.Buffer)
		require.True(t, ok)
		assert.Equal(t, sortLines(expected), sortLines(buf.String()), "wrong %s result in run %d", name, runNo)
	}

	checkRun := func(runNo, wantChecks, wantErrors int, want wantType) {
		accounting.GlobalStats().ResetCounters()
		buf := new(bytes.Buffer)
		log.SetOutput(buf)
		defer log.SetOutput(os.Stderr)

		opt := operations.CheckOpt{
			Combined:     new(bytes.Buffer),
			Match:        new(bytes.Buffer),
			Differ:       new(bytes.Buffer),
			Error:        new(bytes.Buffer),
			MissingOnSrc: new(bytes.Buffer),
			MissingOnDst: new(bytes.Buffer),
		}
		err := operations.CheckSum(ctx, dataFs, r.Fremote, sumFile, hashType, &opt, download)

		gotErrors := int(accounting.GlobalStats().GetErrors())
		if wantErrors == 0 {
			assert.NoError(t, err, "unexpected error in run %d", runNo)
		}
		if wantErrors > 0 {
			assert.Error(t, err, "no expected error in run %d", runNo)
		}
		assert.Equal(t, wantErrors, gotErrors, "wrong error count in run %d", runNo)

		gotChecks := int(accounting.GlobalStats().GetChecks())
		if wantChecks > 0 || gotChecks > 0 {
			assert.Contains(t, buf.String(), "matching files", "missing matching files in run %d", runNo)
		}
		assert.Equal(t, wantChecks, gotChecks, "wrong number of checks in run %d", runNo)

		checkResult(runNo, want, "combined", opt.Combined)
		checkResult(runNo, want, "missingonsrc", opt.MissingOnSrc)
		checkResult(runNo, want, "missingondst", opt.MissingOnDst)
		checkResult(runNo, want, "match", opt.Match)
		checkResult(runNo, want, "differ", opt.Differ)
		checkResult(runNo, want, "error", opt.Error)
	}

	check := func(runNo, wantChecks, wantErrors int, wantResults wantType) {
		runName := fmt.Sprintf("move%d", runNo)
		t.Run(runName, func(t *testing.T) {
			checkRun(runNo, wantChecks, wantErrors, wantResults)
		})
	}

	file1 := makeFile("banana", testString1)
	fcsums := makeSums(operations.HashSums{
		"banana": testDigest1,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1)
	check(1, 1, 0, wantType{
		"combined":     "= banana\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "banana\n",
		"differ":       "",
		"error":        "",
	})

	file2 := makeFile("potato", testString2)
	fcsums = makeSums(operations.HashSums{
		"banana": testDigest1,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1, file2)
	check(2, 2, 1, wantType{
		"combined":     "- potato\n= banana\n",
		"missingonsrc": "potato\n",
		"missingondst": "",
		"match":        "banana\n",
		"differ":       "",
		"error":        "",
	})

	fcsums = makeSums(operations.HashSums{
		"banana": testDigest1,
		"potato": testDigest2,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1, file2)
	check(3, 2, 0, wantType{
		"combined":     "= potato\n= banana\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "banana\npotato\n",
		"differ":       "",
		"error":        "",
	})

	fcsums = makeSums(operations.HashSums{
		"banana": testDigest2,
		"potato": testDigest2,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1, file2)
	check(4, 2, 1, wantType{
		"combined":     "* banana\n= potato\n",
		"missingonsrc": "",
		"missingondst": "",
		"match":        "potato\n",
		"differ":       "banana\n",
		"error":        "",
	})

	fcsums = makeSums(operations.HashSums{
		"banana": testDigest1,
		"potato": testDigest2,
		"orange": testDigest2,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1, file2)
	check(5, 2, 1, wantType{
		"combined":     "+ orange\n= potato\n= banana\n",
		"missingonsrc": "",
		"missingondst": "orange\n",
		"match":        "banana\npotato\n",
		"differ":       "",
		"error":        "",
	})

	fcsums = makeSums(operations.HashSums{
		"banana": testDigest1,
		"potato": testDigest1,
		"orange": testDigest2,
	})
	fstest.CheckItems(t, r.Fremote, fcsums, file1, file2)
	check(6, 2, 2, wantType{
		"combined":     "+ orange\n* potato\n= banana\n",
		"missingonsrc": "",
		"missingondst": "orange\n",
		"match":        "banana\n",
		"differ":       "potato\n",
		"error":        "",
	})
}

func TestCheckSum(t *testing.T) {
	testCheckSum(t, false)
}

func TestCheckSumDownload(t *testing.T) {
	testCheckSum(t, true)
}
