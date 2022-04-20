package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/inconshreveable/log15"

	"github.com/sourcegraph/sourcegraph/internal/conf/reposource"
	"github.com/sourcegraph/sourcegraph/internal/vcs"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

type packageSyncer interface {
	fetchDependencies(repoName string) ([]reposource.PackageDependency, error)
	fetch(dir string) (io.ReadCloser, error)
	unpack(r io.ReadCloser) error
	placeholderDependency() reposource.PackageDependency

	// Skip not clonable instead of hard failure, Take out interface
	IsCloneable(ctx context.Context, remoteURL *vcs.URL) error
}

func newVCSPackageSyncer(typ string, ps packageSyncer) (VCSSyncer, error) {
	return &vcsPackageSyncer{
		typ:           typ,
		packageSyncer: ps,
	}, nil
}

type vcsPackageSyncer struct {
	typ string
	packageSyncer
}

func (ps *vcsPackageSyncer) Type() string {
	return ps.typ
}

func (ps *vcsPackageSyncer) RemoteShowCommand(ctx context.Context, remoteURL *vcs.URL) (cmd *exec.Cmd, err error) {
	return exec.CommandContext(ctx, "git", "remote", "show", "./"), nil
}

func (ps *vcsPackageSyncer) CloneCommand(ctx context.Context, remoteURL *vcs.URL, bareGitDirectory string) (*exec.Cmd, error) {
	err := os.MkdirAll(bareGitDirectory, 0755)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "git", "--bare", "init")
	if _, err := runCommandInDirectory(ctx, cmd, bareGitDirectory, ps.placeholderDependency()); err != nil {
		return nil, err
	}

	// The Fetch method is responsible for cleaning up temporary directories.
	if err := ps.Fetch(ctx, remoteURL, GitDir(bareGitDirectory)); err != nil {
		return nil, errors.Wrapf(err, "failed to fetch repo for %s", remoteURL)
	}

	// no-op command to satisfy VCSSyncer interface, see docstring for more details.
	return exec.CommandContext(ctx, "git", "--version"), nil
}

func (ps *vcsPackageSyncer) Fetch(ctx context.Context, remoteURL *vcs.URL, dir GitDir) error {
	dependencies, err := ps.fetchDependencies(remoteURL.Path)
	if err != nil {
		return err
	}

	out, err := runCommandInDirectory(ctx, exec.CommandContext(ctx, "git", "tag"), string(dir), placeholderGoDependency)
	if err != nil {
		return err
	}

	tags := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) == 0 {
			continue
		}
		tags[line] = true
	}

	for i, dependency := range dependencies {
		if tags[dependency.GitTagFromVersion()] {
			continue
		}
		// the gitPushDependencyTag method is responsible for cleaning up temporary directories.
		if err := ps.gitPushDependencyTag(ctx, string(dir), dependency, i == 0); err != nil {
			return errors.Wrapf(err, "error pushing dependency %q", dependency.PackageManagerSyntax())
		}
	}

	dependencyTags := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		dependencyTags[dependency.GitTagFromVersion()] = struct{}{}
	}

	for tag := range tags {
		if _, isDependencyTag := dependencyTags[tag]; !isDependencyTag {
			cmd := exec.CommandContext(ctx, "git", "tag", "-d", tag)
			if _, err := runCommandInDirectory(ctx, cmd, string(dir), placeholderGoDependency); err != nil {
				log15.Error("Failed to delete git tag", "error", err, "tag", tag)
				continue
			}
		}
	}

	return nil
}

func (ps *vcsPackageSyncer) gitPushDependencyTag(ctx context.Context, bareGitDirectory string, dep reposource.PackageDependency, isLatestVersion bool) error {
	workDir, err := os.MkdirTemp("", ps.Type())
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	r, err := ps.fetch(workDir)
	if err != nil {
		return err
	}

	err = ps.unpack(r)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "init")
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "git", "add", ".")
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
	cmd = exec.CommandContext(ctx, "git", "commit", "--no-verify",
		"-m", dep.PackageManagerSyntax(), "--date", stableGitCommitDate)
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "git", "tag",
		"-m", dep.PackageManagerSyntax(), dep.GitTagFromVersion())
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "git", "remote", "add", "origin", bareGitDirectory)
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
	cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "--force", "origin", "--tags")
	if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
		return err
	}

	if isLatestVersion {
		defaultBranch, err := runCommandInDirectory(ctx, exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD"), workDir, dep)
		if err != nil {
			return err
		}
		// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
		cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "--force", "origin", strings.TrimSpace(defaultBranch)+":latest", dep.GitTagFromVersion())
		if _, err := runCommandInDirectory(ctx, cmd, workDir, dep); err != nil {
			return err
		}
	}

	return nil
}

func runCommandInDirectory(ctx context.Context, cmd *exec.Cmd, workingDirectory string, dependency reposource.PackageDependency) (string, error) {
	gitName := dependency.PackageManagerSyntax() + " authors"
	gitEmail := "code-intel@sourcegraph.com"
	cmd.Dir = workingDirectory
	cmd.Env = append(cmd.Env, "EMAIL="+gitEmail)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME="+gitName)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_EMAIL="+gitEmail)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+stableGitCommitDate)
	cmd.Env = append(cmd.Env, "GIT_COMMITTER_NAME="+gitName)
	cmd.Env = append(cmd.Env, "GIT_COMMITTER_EMAIL="+gitEmail)
	cmd.Env = append(cmd.Env, "GIT_COMMITTER_DATE="+stableGitCommitDate)
	output, err := runWith(ctx, cmd, false, nil)
	if err != nil {
		return "", errors.Wrapf(err, "command %s failed with output %s", cmd.Args, string(output))
	}
	return string(output), nil
}

func isPotentiallyMaliciousFilepathInArchive(filepath, destinationDir string) (outputPath string, _ bool) {
	if strings.HasSuffix(filepath, "/") {
		// Skip directory entries. Directory entries must end
		// with a forward slash (even on Windows) according to
		// `file.Name` docstring.
		return "", true
	}

	if strings.HasPrefix(filepath, "/") {
		// Skip absolute paths. While they are extracted relative to `destination`,
		// they should be unimportant. Related issue https://github.com/golang/go/issues/48085#issuecomment-912659635
		return "", true
	}

	for _, dirEntry := range strings.Split(filepath, string(os.PathSeparator)) {
		if dirEntry == ".git" {
			// For security reasons, don't unzip files under any `.git/`
			// directory. See https://github.com/sourcegraph/security-issues/issues/163
			return "", true
		}
	}

	cleanedOutputPath := path.Join(destinationDir, filepath)
	if !strings.HasPrefix(cleanedOutputPath, destinationDir) {
		// For security reasons, skip file if it's not a child
		// of the target directory. See "Zip Slip Vulnerability".
		return "", true
	}

	return cleanedOutputPath, false
}

// [NOTE: LSIF-config-json]
//
// For JVM languages, when we create a fake Git repository from a Maven module
// we also add a lsif-java.json file to the repository. However, we don't create
// an analogous lsif-node.json for JavaScript/TypeScript. Here's why:
//
// 1. A specific JDK version is needed to correctly index the code. This JDK
//    version needs to be specified when launching lsif-java. So if we wanted
//    to determine the JDK version at auto-indexing time (instead of at upload
//    time), we'd need to have a separate tool that ran before lsif-java.
//
//    This doesn't apply to JS/TS because there is no clear source of truth for
//    the version of the runtime (Node etc.) that is needed.
//
// 2. The lsif-java.json file indicates whether the repo contains the sources of
//    the JDK or whether it's a regular Maven artifact. For the JDK, lsif-java
//    has a special case to emit "export" monikers instead of "import".
//
//    This doesn't apply to JS/S because there is no special npm module
//    analogous to the JDK.
//
// 3. The lsif-java.json file is used as a marker file to enable inference
//    of the auto-indexing configuration for a JVM package repo
//    (e.g. from Maven Central). Since JVM source repos (e.g. from GitHub) lack
//    this file, the auto-indexing configuration is not inferred.
//
//    This doesn't apply to JS/TS because auto-indexing configuration is
//    inferred from the package.json file for both source repos and package
//    repos.
