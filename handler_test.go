package gitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/inconshreveable/log15"
	git "github.com/lhchavez/git2go"
	"github.com/omegaup/githttp"
	"github.com/omegaup/gitserver/gitservertest"
	"github.com/omegaup/gitserver/request"
	base "github.com/omegaup/go-base"
	"github.com/omegaup/quark/common"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

const (
	userAuthorization     = "Basic dXNlcjp1c2Vy"
	adminAuthorization    = "Basic YWRtaW46YWRtaW4="
	readonlyAuthorization = "Basic cmVhZG9ubHk6cmVhZG9ubHk="
)

var (
	fakeInteractiveSettingsCompiler = &FakeInteractiveSettingsCompiler{
		Settings: nil,
		Err:      errors.New("unsupported"),
	}
)

func authorize(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
	operation githttp.GitOperation,
) (githttp.AuthorizationLevel, string) {
	username, _, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"Git\"")
		w.WriteHeader(http.StatusUnauthorized)
		return githttp.AuthorizationDenied, ""
	}

	requestContext := request.FromContext(ctx)
	if username == "admin" {
		requestContext.IsAdmin = true
		requestContext.CanView = true
		requestContext.CanEdit = true
		return githttp.AuthorizationAllowed, username
	}
	if username == "user" {
		requestContext.CanView = true
		requestContext.CanEdit = true
		return githttp.AuthorizationAllowedRestricted, username
	}
	if username == "readonly" {
		requestContext.CanView = true
		return githttp.AuthorizationAllowedReadOnly, username
	}
	w.WriteHeader(http.StatusForbidden)
	return githttp.AuthorizationDenied, username
}

func getReference(
	t *testing.T,
	problemAlias string,
	refName string,
	ts *httptest.Server,
) *git.Oid {
	prePushURL, err := url.Parse(ts.URL + "/" + problemAlias + "/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatalf("Failed to parse URL: %v", err)
	}
	req := &http.Request{
		Method: "GET",
		URL:    prePushURL,
		Header: map[string][]string{
			"Authorization": {adminAuthorization},
		},
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Failed to create pre-pull request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Failed to request pre-pull: Status %v, headers: %v", res.StatusCode, res.Header)
	}

	pr := githttp.NewPktLineReader(res.Body)

	for {
		line, err := pr.ReadPktLine()
		if err == io.EOF {
			break
		}
		if err == githttp.ErrFlush {
			continue
		}
		tokens := strings.FieldsFunc(
			strings.Trim(string(line), "\n"),
			func(r rune) bool {
				return r == ' ' || r == '\x00'
			},
		)
		if len(tokens) < 2 {
			continue
		}
		if strings.HasPrefix(tokens[0], "#") || tokens[1] != refName {
			continue
		}
		oid, err := git.NewOid(tokens[0])
		if err != nil {
			t.Fatalf("Failed to parse oid %v: %v", tokens[0], err)
		}
		return oid
	}
	return &git.Oid{}
}

func createCommit(
	t *testing.T,
	tmpDir string,
	problemAlias string,
	oldOid *git.Oid,
	contents map[string]io.Reader,
	commitMessage string,
	log log15.Logger,
) (*git.Oid, []byte) {
	repo, err := git.OpenRepository(path.Join(tmpDir, problemAlias+".git"))
	if err != nil {
		t.Fatalf("Failed to open repository: %v", err)
	}
	defer repo.Free()

	var parentCommits []*git.Commit
	if !oldOid.IsZero() {
		var err error
		parentCommit, err := repo.LookupCommit(oldOid)
		if err != nil {
			t.Fatalf("Failed to lookup commit %v: %v", oldOid, err)
		}
		parentCommits = append(parentCommits, parentCommit)
	}

	odb, err := repo.Odb()
	if err != nil {
		t.Fatalf("Failed to open odb: %v", err)
	}
	defer odb.Free()

	mempack, err := git.NewMempack(odb)
	if err != nil {
		t.Fatalf("Failed to create mempack: %v", err)
	}
	defer mempack.Free()

	tree, err := githttp.BuildTree(repo, contents, log)
	if err != nil {
		t.Fatalf("Failed to build tree: %v", err)
	}
	defer tree.Free()

	newCommitID, err := repo.CreateCommit(
		"",
		&git.Signature{
			Name:  "author",
			Email: "author@test.test",
			When:  time.Unix(0, 0),
		},
		&git.Signature{
			Name:  "committer",
			Email: "committer@test.test",
			When:  time.Unix(0, 0),
		},
		commitMessage,
		tree,
		parentCommits...,
	)
	if err != nil {
		t.Fatalf("Failed to create commit: %v", err)
	}

	packContents, err := mempack.Dump(repo)
	if err != nil {
		t.Fatalf("Failed to create mempack: %v", err)
	}

	return newCommitID, packContents
}

func push(
	t *testing.T,
	tmpDir string,
	authorization string,
	problemAlias string,
	refName string,
	oldOid, newOid *git.Oid,
	packContents []byte,
	expectedResponse []githttp.PktLineResponse,
	ts *httptest.Server,
) {
	var inBuf bytes.Buffer

	{
		// Taken from git 2.14.1
		pw := githttp.NewPktLineWriter(&inBuf)
		pw.WritePktLine([]byte(fmt.Sprintf(
			"%s %s %s\x00report-status\n",
			oldOid.String(),
			newOid.String(),
			refName,
		)))

		if len(packContents) > 0 {
			pw.Flush()
			if _, err := inBuf.Write(packContents); err != nil {
				t.Fatalf("Failed to write packfile: %v", err)
			}
		}
	}

	pushURL, err := url.Parse(ts.URL + "/" + problemAlias + "/git-receive-pack")
	if err != nil {
		t.Fatalf("Failed to parse URL: %v", err)
	}
	req := &http.Request{
		Method: "POST",
		URL:    pushURL,
		Body:   ioutil.NopCloser(&inBuf),
		Header: map[string][]string{
			"Authorization": {authorization},
		},
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Failed to create pre-push request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusForbidden {
		t.Fatalf("Failed to request pre-push: Status %v, headers: %v", res.StatusCode, res.Header)
	}

	if actual, ok := githttp.ComparePktLineResponse(res.Body, expectedResponse); !ok {
		t.Errorf("push expected %q, got %q", expectedResponse, actual)
	}
}

func TestInvalidRef(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(authorize, nil, false, OverallWallTimeHardLimit, fakeInteractiveSettingsCompiler, log),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	{
		repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
		if err != nil {
			t.Fatalf("Failed to initialize git repository: %v", err)
		}
		repo.Free()
	}

	newOid, packContents := createCommit(
		t,
		tmpDir,
		problemAlias,
		&git.Oid{},
		map[string]io.Reader{
			"settings.json":          strings.NewReader(gitservertest.DefaultSettingsJSON),
			"cases/0.in":             strings.NewReader("1 2"),
			"cases/0.out":            strings.NewReader("3"),
			"statements/es.markdown": strings.NewReader("Sumas"),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		userAuthorization,
		problemAlias,
		"refs/heads/private",
		&git.Oid{}, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/heads/private read-only\n", Err: nil},
		},
		ts,
	)
	push(
		t,
		tmpDir,
		userAuthorization,
		problemAlias,
		"refs/heads/arbitrarybranchname",
		&git.Oid{}, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/heads/arbitrarybranchname invalid-ref\n", Err: nil},
		},
		ts,
	)
}

func TestDelete(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(authorize, nil, false, OverallWallTimeHardLimit, fakeInteractiveSettingsCompiler, log),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	{
		repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
		if err != nil {
			t.Fatalf("Failed to initialize git repository: %v", err)
		}
		repo.Free()
	}

	{
		newOid, packContents := createCommit(
			t,
			tmpDir,
			problemAlias,
			&git.Oid{},
			map[string]io.Reader{
				"settings.json":          strings.NewReader(gitservertest.DefaultSettingsJSON),
				"cases/0.in":             strings.NewReader("1 2"),
				"cases/0.out":            strings.NewReader("3"),
				"statements/es.markdown": strings.NewReader("Sumas"),
			},
			"Initial commit",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/changes/initial",
			&git.Oid{}, newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/changes/initial\n", Err: nil},
			},
			ts,
		)
	}
	push(
		t,
		tmpDir,
		userAuthorization,
		problemAlias,
		"refs/changes/initial",
		getReference(t, problemAlias, "refs/changes/initial", ts),
		&git.Oid{},
		githttp.EmptyPackfile,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/changes/initial delete-unallowed\n", Err: nil},
		},
		ts,
	)
}

func TestServerCreateReview(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(authorize, nil, false, OverallWallTimeHardLimit, fakeInteractiveSettingsCompiler, log),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	{
		repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
		if err != nil {
			t.Fatalf("Failed to initialize git repository: %v", err)
		}
		repo.Free()
	}

	// Create code review
	{
		newOid, packContents := createCommit(
			t,
			tmpDir,
			problemAlias,
			&git.Oid{},
			map[string]io.Reader{
				"settings.json":          strings.NewReader(gitservertest.DefaultSettingsJSON),
				"cases/0.in":             strings.NewReader("1 2"),
				"cases/0.out":            strings.NewReader("3"),
				"statements/es.markdown": strings.NewReader("Sumas"),
			},
			"Initial commit",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/changes/initial",
			&git.Oid{}, newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/changes/initial\n", Err: nil},
			},
			ts,
		)
	}

	// Try a few invalid publish paths
	{
		// User is not an administrator, so they cannot change refs/heads/master.
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/heads/master",
			getReference(t, problemAlias, "refs/heads/master", ts),
			getReference(t, problemAlias, "refs/changes/initial", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/heads/master forbidden\n", Err: nil},
			},
			ts,
		)
		// User is not an administrator, so they cannot change refs/heads/published.
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/heads/published",
			getReference(t, problemAlias, "refs/heads/published", ts),
			getReference(t, problemAlias, "refs/changes/initial", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/heads/published forbidden\n", Err: nil},
			},
			ts,
		)
		// User is an administrator, but cannot point refs/heads/published to
		// something that's not a commit in master.
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/published",
			getReference(t, problemAlias, "refs/heads/published", ts),
			getReference(t, problemAlias, "refs/changes/initial", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/heads/published published-must-point-to-commit-in-master\n", Err: nil},
			},
			ts,
		)
	}

	// Publish initial review
	{
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/master",
			getReference(t, problemAlias, "refs/heads/master", ts),
			getReference(t, problemAlias, "refs/changes/initial", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/heads/master\n", Err: nil},
			},
			ts,
		)
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/published",
			getReference(t, problemAlias, "refs/heads/published", ts),
			getReference(t, problemAlias, "refs/heads/master", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/heads/published\n", Err: nil},
			},
			ts,
		)
	}

	// Create new revision
	{
		newOid, packContents := createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/heads/master", ts),
			map[string]io.Reader{
				"settings.json":          strings.NewReader(gitservertest.DefaultSettingsJSON),
				"cases/0.in":             strings.NewReader("3 2"),
				"cases/0.out":            strings.NewReader("1"),
				"statements/es.markdown": strings.NewReader("Restas"),
			},
			"Initial commit",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/changes/initial2",
			&git.Oid{}, newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/changes/initial2\n", Err: nil},
			},
			ts,
		)
	}

	// Send out a few invalid code reviews.
	{
		newOid, packContents := createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{},
			"Initial commit",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: iteration uuid in commit message missing or malformed\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"should/not/have/had/trees": strings.NewReader("\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: refs/meta/review must have a flat tree\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("missing trailing newline"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: ledger does not end in newline\n", Err: nil},
			},
			ts,
		)

		reviewCommitHash := getReference(t, problemAlias, "refs/changes/initial2", ts).String()
		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				reviewCommitHash: strings.NewReader("{}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: missing ledger file\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("non-JSON ledger\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review json-parse-error: appended ledger contents: invalid character 'o' in literal null (expecting 'u')\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("{}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: invalid iteration uuid in ledger entry\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000001\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/meta/review review-bad-layout: invalid iteration uuid in ledger entry\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("non-JSON entry\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{
					Line: fmt.Sprintf(
						"ng refs/meta/review review-bad-layout: malformed appended comment in %s: invalid character 'o' in literal null (expecting 'u')\n",
						reviewCommitHash,
					),
					Err: nil,
				},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"bar\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: invalid author in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: invalid iteration uuid in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: missing or malformed comment uuid in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: duplicate comment uuid in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"missing\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: file 'missing' not found in %s: the path 'missing' does not exist in the given tree\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\",\"parentUuid\":\"\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: parent uuid missing in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000002\",\"parentUuid\":\"00000000-0000-0000-0000-000000000001\",\"range\":{\"lineStart\":0,\"lineEnd\":0,\"colStart\":0,\"colEnd\":0}}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: cannot specify both parentUuid and range in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: empty comment message in %s\n", reviewCommitHash), Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger":         strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000000",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/meta/review\n", Err: nil},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n" +
					"{\"uuid\":\"00000000-0000-0000-0000-000000000001\",\"author\":\"bar\",\"date\":1,\"Summary\":\"Good!\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000001",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{
					Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: failed to find %s in review iteration\n", reviewCommitHash),
					Err:  nil,
				},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n" +
					"{\"uuid\":\"00000000-0000-0000-0000-000000000001\",\"author\":\"bar\",\"date\":1,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"gaslighting!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n" +
					"{\"author\":\"bar\",\"date\":0,\"done\":true,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000001\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000002\",\"parentUuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000001",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{
					Line: fmt.Sprintf("ng refs/meta/review review-bad-layout: unexpected non-append to %s\n", reviewCommitHash),
					Err:  nil,
				},
			},
			ts,
		)

		newOid, packContents = createCommit(
			t,
			tmpDir,
			problemAlias,
			getReference(t, problemAlias, "refs/meta/review", ts),
			map[string]io.Reader{
				"ledger": strings.NewReader("{\"uuid\":\"00000000-0000-0000-0000-000000000000\",\"author\":\"foo\",\"date\":0,\"Summary\":\"Good!\"}\n" +
					"{\"uuid\":\"00000000-0000-0000-0000-000000000001\",\"author\":\"bar\",\"date\":1,\"Summary\":\"Good!\"}\n"),
				reviewCommitHash: strings.NewReader("{\"author\":\"foo\",\"date\":0,\"done\":false,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000000\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000001\"}\n" +
					"{\"author\":\"bar\",\"date\":0,\"done\":true,\"filename\":\"cases/0.in\",\"iterationUuid\":\"00000000-0000-0000-0000-000000000001\",\"message\":\"Good!\",\"uuid\":\"00000000-0000-0000-0000-000000000002\",\"parentUuid\":\"00000000-0000-0000-0000-000000000001\"}\n"),
			},
			"Foo\n\nIteration: 00000000-0000-0000-0000-000000000001",
			log,
		)
		push(
			t,
			tmpDir,
			userAuthorization,
			problemAlias,
			"refs/meta/review",
			getReference(t, problemAlias, "refs/meta/review", ts),
			newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/meta/review\n", Err: nil},
			},
			ts,
		)
	}

	// Try a few more invalid publish paths
	{
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/published",
			getReference(t, problemAlias, "refs/heads/published", ts),
			getReference(t, problemAlias, "refs/changes/initial2", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ng refs/heads/published published-must-point-to-commit-in-master\n", Err: nil},
			},
			ts,
		)
	}

	// Publish second version
	{
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/master",
			getReference(t, problemAlias, "refs/heads/master", ts),
			getReference(t, problemAlias, "refs/changes/initial2", ts),
			githttp.EmptyPackfile,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/heads/master\n", Err: nil},
			},
			ts,
		)
	}
}

func TestPushGitbomb(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(authorize, nil, false, OverallWallTimeHardLimit, fakeInteractiveSettingsCompiler, log),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	{
		repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
		if err != nil {
			t.Fatalf("Failed to initialize git repository: %v", err)
		}
		repo.Free()
	}

	repo, err := git.OpenRepository(path.Join(tmpDir, problemAlias+".git"))
	if err != nil {
		t.Fatalf("Failed to open repository: %v", err)
	}
	defer repo.Free()

	odb, err := repo.Odb()
	if err != nil {
		t.Fatalf("Failed to open odb: %v", err)
	}
	defer odb.Free()

	mempack, err := git.NewMempack(odb)
	if err != nil {
		t.Fatalf("Failed to create mempack: %v", err)
	}
	defer mempack.Free()

	oid, err := repo.CreateBlobFromBuffer([]byte{})
	if err != nil {
		t.Fatalf("Failed to create blob: %v", err)
	}

	fileMode := git.Filemode(0100644)
	for i := 0; i < 24; i++ {
		log.Debug("Creating gitbomb", "iteration", i)
		treebuilder, err := repo.TreeBuilder()
		if err != nil {
			t.Fatalf("Failed to create TreeBuilder: %v", err)
		}

		for _, filename := range []string{"0", "1"} {
			if err = treebuilder.Insert(filename, oid, fileMode); err != nil {
				t.Fatalf("Failed to insert into TreeBuilder: %v", err)
			}
		}
		oid, err = treebuilder.Write()
		if err != nil {
			t.Fatalf("Failed to write tree: %v", err)
		}
		treebuilder.Free()
		fileMode = 040000
	}

	tree, err := repo.LookupTree(oid)
	if err != nil {
		t.Fatalf("Failed to lookup tree: %v", err)
	}

	log.Debug("Tree looked up")

	newCommitID, err := repo.CreateCommit(
		"",
		&git.Signature{
			Name:  "author",
			Email: "author@test.test",
			When:  time.Unix(0, 0),
		},
		&git.Signature{
			Name:  "committer",
			Email: "committer@test.test",
			When:  time.Unix(0, 0),
		},
		"Initial commit",
		tree,
	)
	if err != nil {
		t.Fatalf("Failed to create commit: %v", err)
	}

	packContents, err := mempack.Dump(repo)
	if err != nil {
		t.Fatalf("Failed to create mempack: %v", err)
	}
	push(
		t,
		tmpDir,
		userAuthorization,
		problemAlias,
		"refs/changes/initial",
		&git.Oid{}, newCommitID,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/changes/initial too-many-objects-in-packfile\n", Err: nil},
		},
		ts,
	)
}

func TestConfig(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(authorize, nil, false, OverallWallTimeHardLimit, fakeInteractiveSettingsCompiler, log),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	{
		repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
		if err != nil {
			t.Fatalf("Failed to initialize git repository: %v", err)
		}
		repo.Free()
	}

	// Normal mirror update.
	oldOid := &git.Oid{}
	newOid, packContents := createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"mirror",
					"repository":"https://github.com/omegaup/test.git"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		userAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config restricted-ref\n", Err: nil},
		},
		ts,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ok refs/meta/config\n", Err: nil},
		},
		ts,
	)

	// Normal subdirectory update.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"subdirectory",
					"repository":"https://github.com/omegaup/test.git",
					"target":"subdirectory"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ok refs/meta/config\n", Err: nil},
		},
		ts,
	)

	// Empty tree.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ok refs/meta/config\n", Err: nil},
		},
		ts,
	)

	// Extra files.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"garbage.txt": strings.NewReader(""),
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"mirror",
					"repository":"https://github.com/omegaup/test.git"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config config-bad-layout: refs/meta/config can only contain a single config.json file\n", Err: nil},
		},
		ts,
	)

	// Wrong filename.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.txt": strings.NewReader(`{
				"publishing":{
					"mode":"mirror",
					"repository":"https://github.com/omegaup/test.git"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config config-bad-layout: refs/meta/config can only contain a single config.json file\n", Err: nil},
		},
		ts,
	)

	// Wrong format.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader("invalid json"),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config json-parse-error: config.json: invalid character 'i' looking for beginning of value\n", Err: nil},
		},
		ts,
	)

	// Wrong publishing mode.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"invalid"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config config-invalid-publishing-mode\n", Err: nil},
		},
		ts,
	)

	// Repository is not an absolute URL.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"mirror",
					"repository":"invalid"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config config-repository-not-absolute-url\n", Err: nil},
		},
		ts,
	)

	// Missing target for subdirectory.
	oldOid = getReference(t, problemAlias, "refs/meta/config", ts)
	newOid, packContents = createCommit(
		t,
		tmpDir,
		problemAlias,
		oldOid,
		map[string]io.Reader{
			"config.json": strings.NewReader(`{
				"publishing":{
					"mode":"subdirectory",
					"repository":"https://github.com/omegaup/test.git"
				}
			}`),
		},
		"Initial commit",
		log,
	)
	push(
		t,
		tmpDir,
		adminAuthorization,
		problemAlias,
		"refs/meta/config",
		oldOid, newOid,
		packContents,
		[]githttp.PktLineResponse{
			{Line: "unpack ok\n", Err: nil},
			{Line: "ng refs/meta/config config-subdirectory-missing-target\n", Err: nil},
		},
		ts,
	)
}

func getProblemDistribSettings(repo *git.Repository, tree *git.Tree) (*common.LiteralInput, error) {
	settingsJSONEntry, err := tree.EntryByPath("settings.distrib.json")
	if err != nil {
		return nil, err
	}
	settingsJSONBlob, err := repo.LookupBlob(settingsJSONEntry.Id)
	if err != nil {
		return nil, err
	}
	defer settingsJSONBlob.Free()

	var settings common.LiteralInput
	if err := json.Unmarshal(settingsJSONBlob.Contents(), &settings); err != nil {
		return nil, err
	}
	return &settings, nil
}

func TestInteractive(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "handler_test")
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}
	if os.Getenv("PRESERVE") == "" {
		defer os.RemoveAll(tmpDir)
	}

	log := base.StderrLog()
	ts := httptest.NewServer(GitHandler(
		tmpDir,
		NewGitProtocol(
			authorize,
			nil,
			true,
			OverallWallTimeHardLimit,
			&FakeInteractiveSettingsCompiler{
				Settings: &common.InteractiveSettings{
					Interfaces:            map[string]map[string]*common.InteractiveInterface{},
					Main:                  "",
					ModuleName:            "",
					ParentLang:            "",
					LibinteractiveVersion: "0.0",
				},
				Err: nil,
			},
			log,
		),
		log,
	))
	defer ts.Close()

	problemAlias := "sumas"

	repo, err := InitRepository(path.Join(tmpDir, problemAlias+".git"))
	if err != nil {
		t.Fatalf("Failed to initialize git repository: %v", err)
	}
	defer repo.Free()

	{
		newOid, packContents := createCommit(
			t,
			tmpDir,
			problemAlias,
			&git.Oid{},
			map[string]io.Reader{
				"settings.json":          strings.NewReader(gitservertest.DefaultSettingsJSON),
				"cases/0.in":             strings.NewReader("1 2"),
				"cases/0.out":            strings.NewReader("3"),
				"statements/es.markdown": strings.NewReader("Sumas"),
				"interactive/sums.idl": strings.NewReader(`// sums.idl
interface Main {
};

interface sums {
	int sums(int a, int b);
};`),
				"interactive/Main.cpp": strings.NewReader(`// Main.cpp
#include <stdio.h>
#include "sums.h"

int main(int argc, char* argv[]) {
	int a, b;
	scanf("%d %d\n", &a, &b);
	printf("%d\n", sums(a, b));
}`),
				"interactive/Main.distrib.cpp": strings.NewReader(`// Main.cpp
#include <stdio.h>
#include "sums.h"

int main(int argc, char* argv[]) {
	// Este es un ejemplo.
	int a, b;
	scanf("%d %d\n", &a, &b);
	printf("%d\n", sums(a, b));
}`),
				"interactive/examples/sample.in":  strings.NewReader("0 1"),
				"interactive/examples/sample.out": strings.NewReader("1"),
			},
			"Initial commit",
			log,
		)
		push(
			t,
			tmpDir,
			adminAuthorization,
			problemAlias,
			"refs/heads/master",
			&git.Oid{}, newOid,
			packContents,
			[]githttp.PktLineResponse{
				{Line: "unpack ok\n", Err: nil},
				{Line: "ok refs/heads/master\n", Err: nil},
			},
			ts,
		)
	}

	masterCommit, err := repo.LookupCommit(
		getReference(t, problemAlias, "refs/heads/master", ts),
	)
	if err != nil {
		t.Fatalf("Failed to lookup commit: %v", err)
	}
	defer masterCommit.Free()
	masterTree, err := masterCommit.Tree()
	if err != nil {
		t.Fatalf("Failed to lookup tree: %v", err)
	}
	defer masterTree.Free()

	problemSettings, err := getProblemSettings(
		repo,
		masterTree,
	)
	if err != nil {
		t.Fatalf("failed to get problem settings: %v", err)
	}
	if problemSettings.Interactive == nil {
		t.Fatalf("Failed to produce interactive settings")
	}

	problemDistribSettings, err := getProblemDistribSettings(
		repo,
		masterTree,
	)
	if err != nil {
		t.Fatalf("failed to get problem distributable settings: %v", err)
	}
	if problemSettings.Limits != *problemDistribSettings.Limits {
		t.Errorf("limits expected %q, got %q", problemSettings.Limits, *problemDistribSettings.Limits)
	}
	if problemDistribSettings.Interactive == nil {
		t.Fatalf("Failed to produce interactive settings")
	}
}