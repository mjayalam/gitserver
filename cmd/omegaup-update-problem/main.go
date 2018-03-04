package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/inconshreveable/log15"
	git "github.com/lhchavez/git2go"
	"github.com/omegaup/githttp"
	"github.com/omegaup/gitserver"
	"github.com/omegaup/gitserver/request"
	base "github.com/omegaup/go-base"
	"github.com/omegaup/quark/common"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"time"
)

var (
	author              = flag.String("author", "", "Author of the commit")
	commitMessage       = flag.String("commit-message", "", "Commit message")
	repositoryPath      = flag.String("repository-path", "", "Path of the git repository")
	problemSettingsJSON = flag.String("problem-settings", "", "(Optional) JSON-encoded ProblemSettings")

	// Flags that are used when updating a repository with a .zip.
	zipPath            = flag.String("zip-path", "", "Path of the .zip file")
	updateCases        = flag.Bool("update-cases", true, "Update cases")
	updateStatements   = flag.Bool("update-statements", true, "Update statements")
	acceptsSubmissions = flag.Bool("accepts-submissions", true, "Problem accepts submissions")
	libinteractivePath = flag.String("libinteractive-path", "/usr/share/java/libinteractive.jar", "Path of libinteractive.jar")

	// Flags that are used when updating a repository with a []BlobUpdate.
	blobUpdateJSON = flag.String("blob-update", "", "Update a subset of the blobs")
)

// UpdateResult represents the result of running this command.
type UpdateResult struct {
	Status      string               `json:"status"`
	Error       string               `json:"error,omitempty"`
	UpdatedRefs []githttp.UpdatedRef `json:"updated_refs,omitempty"`
}

// BlobUpdate represents updating a single blob in the repository.
type BlobUpdate struct {
	Path         string `json:"path"`
	ContentsPath string `json:"contents_path"`
}

func commitZipFile(
	zipPath string,
	repo *git.Repository,
	lockfile *githttp.Lockfile,
	authorUsername string,
	commitMessage string,
	problemSettings *common.ProblemSettings,
	updateMask gitserver.ConvertZipUpdateMask,
	acceptsSubmissions bool,
	log log15.Logger,
) (*UpdateResult, error) {
	zipReader, err := zip.OpenReader(zipPath)
	if err != nil {
		log.Error("Failed to open the zip file", "err", err)
		return nil, err
	}
	defer zipReader.Close()

	oldOid := &git.Oid{}
	var reference *git.Reference
	if ok, _ := repo.IsHeadUnborn(); !ok {
		reference, err = repo.Head()
		if err != nil {
			log.Error("Failed to get the repository's HEAD", "err", err)
			return nil, err
		}
		defer reference.Free()
		oldOid = reference.Target()
	}

	signature := git.Signature{
		Name:  authorUsername,
		Email: fmt.Sprintf("%s@omegaup", authorUsername),
		When:  time.Now(),
	}

	packfile := bytes.NewBuffer([]byte{})
	newOid, err := gitserver.ConvertZipToPackfile(
		&zipReader.Reader,
		problemSettings,
		updateMask,
		repo,
		oldOid,
		&signature,
		&signature,
		commitMessage,
		acceptsSubmissions,
		packfile,
		log,
	)
	if err != nil {
		return nil, err
	}

	ctx := request.NewContext(context.Background())
	requestContext := request.FromContext(ctx)
	requestContext.IsAdmin = true
	requestContext.CanView = true
	requestContext.CanEdit = true

	protocol := gitserver.NewGitProtocol(
		nil,
		nil,
		true,
		gitserver.OverallWallTimeHardLimit,
		&gitserver.LibinteractiveCompiler{
			LibinteractiveJarPath: *libinteractivePath,
			Log:                   log,
		},
		log,
	)
	updatedRefs, err, unpackErr := protocol.PushPackfile(
		ctx,
		repo,
		lockfile,
		githttp.AuthorizationAllowed,
		[]*githttp.GitCommand{
			{
				Old:           oldOid,
				New:           newOid,
				ReferenceName: "refs/heads/master",
				Reference:     reference,
			},
		},
		packfile,
	)
	if err != nil {
		log.Error("Failed to push .zip", "err", err)
		return nil, err
	}
	if unpackErr != nil {
		log.Error("Failed to unpack packfile", "err", unpackErr)
		return nil, err
	}

	return &UpdateResult{
		Status:      "ok",
		UpdatedRefs: updatedRefs,
	}, nil
}

func convertBlobsToPackfile(
	contents map[string]io.Reader,
	repo *git.Repository,
	parent *git.Oid,
	author, committer *git.Signature,
	commitMessage string,
	w io.Writer,
	log log15.Logger,
) (*git.Oid, error) {
	headCommit, err := repo.LookupCommit(parent)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to get the repository's HEAD's commit",
		)
	}
	defer headCommit.Free()

	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to get the repository's HEAD's commit's tree",
		)
	}
	defer headTree.Free()

	odb, err := repo.Odb()
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to open odb",
		)
	}
	defer odb.Free()

	looseObjectsDir, err := ioutil.TempDir("", fmt.Sprintf("loose_objects_%s", path.Base(repo.Path())))
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create temporary directory for loose objects",
		)
	}
	defer os.RemoveAll(looseObjectsDir)

	looseObjectsBackend, err := git.NewOdbBackendLoose(looseObjectsDir, -1, false, 0, 0)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create new loose object backend",
		)
	}
	if err := odb.AddBackend(looseObjectsBackend, 999); err != nil {
		looseObjectsBackend.Free()
		return nil, errors.Wrap(
			err,
			"failed to register loose object backend",
		)
	}

	tree, err := githttp.BuildTree(
		repo,
		contents,
		log,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create new tree",
		)
	}
	defer tree.Free()

	mergedTree, err := githttp.MergeTrees(
		repo,
		log,
		tree,
		headTree,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to merge tree",
		)
	}
	defer mergedTree.Free()

	newCommitID, err := repo.CreateCommit(
		"",
		author,
		committer,
		commitMessage,
		mergedTree,
		headCommit,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create commit",
		)
	}

	walk, err := repo.Walk()
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create revwalk",
		)
	}
	defer walk.Free()

	if err := walk.Hide(headCommit.Id()); err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to hide commit %s", headCommit.Id().String(),
		)
	}
	if err := walk.Push(newCommitID); err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to add commit %s", newCommitID.String(),
		)
	}

	pb, err := repo.NewPackbuilder()
	if err != nil {
		return nil, errors.Wrap(
			err,
			"failed to create packbuilder",
		)
	}
	defer pb.Free()

	if err := pb.InsertWalk(walk); err != nil {
		return nil, errors.Wrap(
			err,
			"failed to insert walk into packbuilder",
		)
	}

	if err := pb.Write(w); err != nil {
		return nil, errors.Wrap(
			err,
			"failed to write packfile",
		)
	}

	return newCommitID, nil
}

func commitBlobs(
	repo *git.Repository,
	lockfile *githttp.Lockfile,
	authorUsername string,
	commitMessage string,
	contents map[string]io.Reader,
	log log15.Logger,
) (*UpdateResult, error) {
	reference, err := repo.Head()
	if err != nil {
		log.Error("Failed to get the repository's HEAD", "err", err)
		return nil, err
	}
	defer reference.Free()
	oldOid := reference.Target()

	signature := git.Signature{
		Name:  authorUsername,
		Email: fmt.Sprintf("%s@omegaup", authorUsername),
		When:  time.Now(),
	}

	var pack bytes.Buffer
	newOid, err := convertBlobsToPackfile(
		contents,
		repo,
		oldOid,
		&signature,
		&signature,
		commitMessage,
		&pack,
		log,
	)
	if err != nil {
		return nil, err
	}

	ctx := request.NewContext(context.Background())
	requestContext := request.FromContext(ctx)
	requestContext.IsAdmin = true
	requestContext.CanView = true
	requestContext.CanEdit = true

	protocol := gitserver.NewGitProtocol(
		nil,
		nil,
		true,
		gitserver.OverallWallTimeHardLimit,
		&gitserver.LibinteractiveCompiler{
			LibinteractiveJarPath: *libinteractivePath,
		},
		log,
	)
	updatedRefs, err, unpackErr := protocol.PushPackfile(
		ctx,
		repo,
		lockfile,
		githttp.AuthorizationAllowed,
		[]*githttp.GitCommand{
			{
				Old:           oldOid,
				New:           newOid,
				ReferenceName: "refs/heads/master",
				Reference:     reference,
			},
		},
		&pack,
	)
	if err != nil {
		log.Error("Failed to push blobs", "err", err)
		return nil, err
	}
	if unpackErr != nil {
		log.Error("Failed to unpack packfile", "err", unpackErr)
		return nil, err
	}

	return &UpdateResult{
		Status:      "ok",
		UpdatedRefs: updatedRefs,
	}, nil
}

func commitSettings(
	repo *git.Repository,
	lockfile *githttp.Lockfile,
	authorUsername string,
	commitMessage string,
	problemSettings *common.ProblemSettings,
	log log15.Logger,
) (*UpdateResult, error) {
	reference, err := repo.Head()
	if err != nil {
		return nil, base.ErrorWithCategory(
			gitserver.ErrInternalGit,
			errors.Wrap(
				err,
				"failed to get the repository's HEAD",
			),
		)
	}
	defer reference.Free()

	parentCommit, err := repo.LookupCommit(reference.Target())
	if err != nil {
		return nil, base.ErrorWithCategory(
			gitserver.ErrInternalGit,
			errors.Wrapf(
				err,
				"failed to find parent commit %s",
				reference.Target().String(),
			),
		)
	}
	defer parentCommit.Free()

	parentTree, err := parentCommit.Tree()
	if err != nil {
		return nil, base.ErrorWithCategory(
			gitserver.ErrInternalGit,
			errors.Wrapf(
				err,
				"failed to find tree for parent commit %s",
				parentCommit,
			),
		)
	}
	defer parentTree.Free()

	// settings.json
	contents := make(map[string]io.Reader)
	{
		var updatedProblemSettings common.ProblemSettings
		entry := parentTree.EntryByName("settings.json")
		if entry == nil {
			return nil, base.ErrorWithCategory(
				gitserver.ErrInternalGit,
				errors.New("failed to find settings.json"),
			)
		}
		blob, err := repo.LookupBlob(entry.Id)
		if err != nil {
			return nil, base.ErrorWithCategory(
				gitserver.ErrInternalGit,
				errors.Wrap(
					err,
					"failed to lookup settings.json",
				),
			)
		}
		defer blob.Free()

		if err := json.Unmarshal(blob.Contents(), &updatedProblemSettings); err != nil {
			return nil, base.ErrorWithCategory(
				gitserver.ErrJSONParseError,
				errors.Wrap(
					err,
					"settings.json",
				),
			)
		}

		if updatedProblemSettings.Validator.Name != problemSettings.Validator.Name {
			if updatedProblemSettings.Validator.Name == "custom" {
				return nil, base.ErrorWithCategory(
					gitserver.ErrProblemBadLayout,
					errors.Errorf(
						"problem with unused validator",
					),
				)
			}
			if problemSettings.Validator.Name == "custom" {
				return nil, base.ErrorWithCategory(
					gitserver.ErrProblemBadLayout,
					errors.Errorf(
						"problem with custom validator missing a validator",
					),
				)
			}
		}

		updatedProblemSettings.Limits = problemSettings.Limits
		updatedProblemSettings.Validator.Name = problemSettings.Validator.Name
		updatedProblemSettings.Validator.Tolerance = problemSettings.Validator.Tolerance
		updatedProblemSettings.Validator.Limits = problemSettings.Validator.Limits

		problemSettingsBytes, err := json.MarshalIndent(&updatedProblemSettings, "", "  ")
		if err != nil {
			return nil, base.ErrorWithCategory(
				gitserver.ErrProblemBadLayout,
				errors.Wrap(
					err,
					"failed to marshal the new settings.json",
				),
			)
		}
		contents["settings.json"] = bytes.NewReader(problemSettingsBytes)
	}

	return commitBlobs(
		repo,
		lockfile,
		authorUsername,
		commitMessage,
		contents,
		log,
	)
}

func main() {
	flag.Parse()
	log := base.StderrLog()

	if *author == "" {
		log.Crit("author cannot be empty. Please specify one with -author")
		os.Exit(1)
	}
	if *commitMessage == "" {
		log.Crit("commit message cannot be empty. Please specify one with -commit-message")
		os.Exit(1)
	}
	if *repositoryPath == "" {
		log.Crit("repository path cannot be empty. Please specify one with -repository-path")
		os.Exit(1)
	}

	var repo *git.Repository
	commitCallback := func() error { return nil }
	if _, err := os.Stat(*repositoryPath); os.IsNotExist(err) {
		dir, err := ioutil.TempDir(filepath.Dir(*repositoryPath), "repository")
		if err != nil {
			log.Crit("Failed to create temporary directory", "err", err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)

		if err := os.Chmod(dir, 0755); err != nil {
			log.Crit("Failed to chmod temporary directory", "err", err)
			os.Exit(1)
		}

		repo, err = gitserver.InitRepository(dir)
		if err != nil {
			log.Crit("Failed to init bare repository", "err", err)
			os.Exit(1)
		}
		commitCallback = func() error {
			return os.Rename(dir, *repositoryPath)
		}
	} else {
		if _, err := os.Stat(path.Join(*repositoryPath, "omegaup/version")); os.IsNotExist(err) {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "\t")
			encoder.Encode(&UpdateResult{
				Status: "error",
				Error:  "omegaup-update-problem-old-version",
			})

			os.Exit(1)
		}
		repo, err = git.OpenRepository(*repositoryPath)
		if err != nil {
			log.Crit("failed to open existing repository", "err", err)
			os.Exit(1)
		}
	}
	defer repo.Free()

	lockfile := githttp.NewLockfile(repo.Path())
	if ok, err := lockfile.TryLock(); !ok {
		log.Info("Waiting for the lockfile", "err", err)
		if err := lockfile.Lock(); err != nil {
			log.Crit("Failed to acquire the lockfile", "err", err)
			os.Exit(1)
		}
	}
	defer lockfile.Unlock()

	var problemSettings *common.ProblemSettings
	if *problemSettingsJSON != "" {
		problemSettings = &common.ProblemSettings{}
		if err := json.Unmarshal([]byte(*problemSettingsJSON), problemSettings); err != nil {
			log.Crit("Failed to parse -problem-settings", "err", err)
			os.Exit(1)
		}
	}

	var updateResult *UpdateResult
	if *zipPath != "" {
		updateMask := gitserver.ConvertZipUpdateMask(0)
		if *updateCases {
			updateMask |= gitserver.ConvertZipUpdateNonStatements
		}
		if *updateStatements {
			updateMask |= gitserver.ConvertZipUpdateStatements
		}

		var err error
		updateResult, err = commitZipFile(
			*zipPath,
			repo,
			lockfile,
			*author,
			*commitMessage,
			problemSettings,
			updateMask,
			*acceptsSubmissions,
			log,
		)
		if err != nil {
			log.Error("Failed update the repository", "path", *repositoryPath, "err", err)
			updateResult = &UpdateResult{
				Status: "error",
				Error:  err.Error(),
			}
		} else if err := commitCallback(); err != nil {
			log.Error("Failed to commit the write to the repository", "err", err)
			updateResult = &UpdateResult{
				Status: "error",
				Error:  err.Error(),
			}
		}
	} else if *blobUpdateJSON != "" {
		var blobUpdates []BlobUpdate
		if err := json.Unmarshal([]byte(*blobUpdateJSON), &blobUpdates); err != nil {
			log.Crit("Failed to parse -blob-update", "err", err)
			os.Exit(1)
		}

		contents := make(map[string]io.Reader)
		for _, blobUpdate := range blobUpdates {
			f, err := os.Open(blobUpdate.ContentsPath)
			if err != nil {
				log.Crit("failed to open blob contents",
					"contents path", blobUpdate.ContentsPath,
					"path", blobUpdate.Path,
				)
				os.Exit(1)
			}
			defer f.Close()
			contents[blobUpdate.Path] = f
		}

		var err error
		updateResult, err = commitBlobs(
			repo,
			lockfile,
			*author,
			*commitMessage,
			contents,
			log,
		)
		if err != nil {
			log.Error("Failed update the repository", "path", *repositoryPath, "err", err)
			updateResult = &UpdateResult{
				Status: "error",
				Error:  err.Error(),
			}
		}
	} else if *problemSettingsJSON != "" {
		var err error
		updateResult, err = commitSettings(
			repo,
			lockfile,
			*author,
			*commitMessage,
			problemSettings,
			log,
		)
		if err != nil {
			log.Error("Failed update the repository", "path", *repositoryPath, "err", err)
			updateResult = &UpdateResult{
				Status: "error",
				Error:  err.Error(),
			}
		}
	} else {
		log.Error("-zip-path, -blob-update, and -problem-settings cannot be simultaneously empty.")
		os.Exit(1)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "\t")
	encoder.Encode(&updateResult)

	if updateResult.Status != "ok" {
		os.Exit(1)
	}
}