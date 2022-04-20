package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	dependenciesStore "github.com/sourcegraph/sourcegraph/internal/codeintel/dependencies/store"
	"github.com/sourcegraph/sourcegraph/internal/conf/reposource"
	"github.com/sourcegraph/sourcegraph/internal/extsvc/jvmpackages/coursier"
	"github.com/sourcegraph/sourcegraph/internal/repos"
	"github.com/sourcegraph/sourcegraph/lib/errors"
	"github.com/sourcegraph/sourcegraph/schema"
)

const (
	// DO NOT CHANGE. This timestamp needs to be stable so that JVM package
	// repos consistently produce the same git revhash. Sourcegraph URLs
	// can optionally include this hash, so changing the timestamp (and hence
	// hashes) will cause existing links to JVM package repos to return 404s.
	stableGitCommitDate = "Thu Apr 8 14:24:52 2021 +0200"

	jvmMajorVersion0 = 44
)

func NewJVMPackagesSyncer(
	connection schema.JVMPackagesConnection,
	store repos.DependenciesStore,
) VCSSyncer {
	// placeholder is used to set GIT_AUTHOR_NAME for git commands
	// that don't create commits or tags. The name of this dependency should never
	// be publicly visible so it can have any random value.
	placeholder, err := reposource.ParseMavenDependency("com.sourcegraph:sourcegraph:1.0.0")
	if err != nil {
		panic(fmt.Sprintf("expected placeholder package to parse but got %v", err))
	}

	var configDeps []string
	if connection.Maven != nil {
		configDeps = connection.Maven.Dependencies
	}

	return &vcsDependenciesSyncer{
		typ:         "jvm_packages",
		scheme:      dependenciesStore.JVMPackagesScheme,
		placeholder: placeholder,
		store:       store,
		configDeps:  configDeps,
		syncer:      &jvmPackagesSyncer{config: &connection},
	}
}

type jvmPackagesSyncer struct {
	config *schema.JVMPackagesConnection
}

func (jvmPackagesSyncer) ParseDependency(dep string) (reposource.PackageDependency, error) {
	return reposource.ParseMavenDependency(dep)
}

func (jvmPackagesSyncer) ParseDependencyFromRepoName(repoName string) (reposource.PackageDependency, error) {
	mod, err := reposource.ParseMavenModule(repoName)
	if err != nil {
		return nil, err
	}
	return &reposource.MavenDependency{MavenModule: mod}, nil
}

func (s *jvmPackagesSyncer) Get(ctx context.Context, name, version string) (reposource.PackageDependency, error) {
	dep, err := reposource.ParseMavenDependency(name + ":" + version)
	if err != nil {
		return nil, err
	}

	err = coursier.Exists(ctx, s.config, dep)
	if err != nil {
		return nil, err
	}

	return dep, nil
}

func (s *jvmPackagesSyncer) Download(ctx context.Context, dir string, dep reposource.PackageDependency) error {
	tgz, err := jvm.FetchSources(ctx, s.client, dep.(*reposource.NpmDependency))
	if err != nil {
		return errors.Wrap(err, "fetch tarball")
	}
	defer tgz.Close()

	if err = decompressTgz(tgz, dir); err != nil {
		return errors.Wrapf(err, "failed to decompress gzipped tarball for %s", dep.PackageManagerSyntax())
	}

	return nil
}

// TODO:
// 1. We need to make sure that coursier.Exists returns errors that can be differentiated between NotFound or everything else.
// 2. Implement Get with coursier.Exists
// 3. Implement Download with FetchSources
// 4. Check that sorting isn't needed and that removing won't break anything
// 5. Manually test.
// 6. Write / refactor tests.

// gitPushDependencyTag pushes a git tag to the given bareGitDirectory path. The
// tag points to a commit that adds all sources of given dependency. When
// isMainBranch is true, the main branch of the bare git directory will also be
// updated to point to the same commit as the git tag.
func (s *JVMPackagesSyncer) gitPushDependencyTag(ctx context.Context, bareGitDirectory string, dependency *reposource.MavenDependency, isLatestVersion bool) error {
	tmpDirectory, err := os.MkdirTemp("", "maven")
	if err != nil {
		return err
	}
	// Always clean up created temporary directories.
	defer os.RemoveAll(tmpDirectory)

	sourceCodeJarPath, err := coursier.FetchSources(ctx, s.Config, dependency)
	if err != nil {
		// Temporary: We shouldn't need both these checks but we're continuing to see the
		// error in production logs which implies `HasType` is not matching.
		if errors.HasType(err, &coursier.ErrNoSources{}) || strings.Contains(err.Error(), "no sources for dependency") {
			// We can't do anything and it's leading to increases in our
			// src_repoupdater_sched_error alert firing more often.
			return nil
		}
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "init")
	if _, err := runCommandInDirectory(ctx, cmd, tmpDirectory, dependency); err != nil {
		return err
	}

	err = s.commitJar(ctx, dependency, tmpDirectory, sourceCodeJarPath, s.Config)
	if err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "git", "remote", "add", "origin", bareGitDirectory)
	if _, err := runCommandInDirectory(ctx, cmd, tmpDirectory, dependency); err != nil {
		return err
	}

	// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
	cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "--force", "origin", "--tags")
	if _, err := runCommandInDirectory(ctx, cmd, tmpDirectory, dependency); err != nil {
		return err
	}

	if isLatestVersion {
		defaultBranch, err := runCommandInDirectory(ctx, exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD"), tmpDirectory, dependency)
		if err != nil {
			return err
		}
		// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
		cmd = exec.CommandContext(ctx, "git", "push", "--no-verify", "--force", "origin", strings.TrimSpace(defaultBranch)+":latest", dependency.GitTagFromVersion())
		if _, err := runCommandInDirectory(ctx, cmd, tmpDirectory, dependency); err != nil {
			return err
		}
	}

	return nil
}

// commitJar creates a git commit in the given working directory that adds all the file contents of the given jar file.
// A `*.jar` file works the same way as a `*.zip` file, it can even be uncompressed with the `unzip` command-line tool.
func (s *JVMPackagesSyncer) commitJar(ctx context.Context, dependency *reposource.MavenDependency,
	workingDirectory, sourceCodeJarPath string, connection *schema.JVMPackagesConnection) error {
	if err := unzipJarFile(sourceCodeJarPath, workingDirectory); err != nil {
		return errors.Wrapf(err, "failed to unzip jar file for %s to %v", dependency.PackageManagerSyntax(), sourceCodeJarPath)
	}

	file, err := os.Create(filepath.Join(workingDirectory, "lsif-java.json"))
	if err != nil {
		return err
	}
	defer file.Close()

	jvmVersion, err := inferJVMVersionFromByteCode(ctx, connection, dependency)
	if err != nil {
		return err
	}

	// See [NOTE: LSIF-config-json] for details on why we use this JSON file.
	jsonContents, err := json.Marshal(&lsifJavaJSON{
		Kind:         dependency.MavenModule.LsifJavaKind(),
		JVM:          jvmVersion,
		Dependencies: dependency.LsifJavaDependencies(),
	})
	if err != nil {
		return err
	}

	_, err = file.Write(jsonContents)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "add", ".")
	if _, err := runCommandInDirectory(ctx, cmd, workingDirectory, dependency); err != nil {
		return err
	}

	// Use --no-verify for security reasons. See https://github.com/sourcegraph/sourcegraph/pull/23399
	cmd = exec.CommandContext(ctx, "git", "commit", "--no-verify", "-m", dependency.PackageManagerSyntax(), "--date", stableGitCommitDate)
	if _, err := runCommandInDirectory(ctx, cmd, workingDirectory, dependency); err != nil {
		return err
	}

	cmd = exec.CommandContext(ctx, "git", "tag", "-m", dependency.PackageManagerSyntax(), dependency.GitTagFromVersion())
	if _, err := runCommandInDirectory(ctx, cmd, workingDirectory, dependency); err != nil {
		return err
	}

	return nil
}

func unzipJarFile(jarPath, destination string) (err error) {
	reader, err := zip.OpenReader(jarPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	destinationDirectory := strings.TrimSuffix(destination, string(os.PathSeparator)) + string(os.PathSeparator)

	for _, file := range reader.File {
		cleanedOutputPath, isPotentiallyMalicious :=
			isPotentiallyMaliciousFilepathInArchive(file.Name, destinationDirectory)
		if isPotentiallyMalicious {
			continue
		}
		err := copyZipFileEntry(file, cleanedOutputPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func copyZipFileEntry(entry *zip.File, outputPath string) (err error) {
	inputFile, err := entry.Open()
	if err != nil {
		return err
	}
	defer func() {
		err1 := inputFile.Close()
		if err == nil {
			err = err1
		}
	}()

	if err = os.MkdirAll(path.Dir(outputPath), 0700); err != nil {
		return err
	}
	outputFile, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err1 := outputFile.Close()
		if err == nil {
			err = err1
		}
	}()

	_, err = io.Copy(outputFile, inputFile)
	return err
}

// inferJVMVersionFromByteCode returns the JVM version that was used to compile
// the bytecode in the given jar file.
func inferJVMVersionFromByteCode(ctx context.Context, connection *schema.JVMPackagesConnection,
	dependency *reposource.MavenDependency) (string, error) {
	if dependency.IsJDK() {
		return dependency.Version, nil
	}

	byteCodeJarPath, err := coursier.FetchByteCode(ctx, connection, dependency)
	if err != nil {
		return "", err
	}
	majorVersionString, err := classFileMajorVersion(byteCodeJarPath)
	if err != nil {
		return "", err
	}
	majorVersion, err := strconv.Atoi(majorVersionString)
	if err != nil {
		return "", errors.Wrapf(err, "failed to convert string %s to int", majorVersion)
	}

	// Java 1.1 (aka "Java 1") has major version 45 and Java 8 has major
	// version 52. To go from the major version of Java version we subtract
	// 44.
	jvmVersion := majorVersion - jvmMajorVersion0

	// The motivation to round the JVM version to the nearst stable release
	// is so that we reduce the number of JDKs on sourcegraph.com. By having
	// fewer JDK versions, features like "find references" will return
	// aggregated results for non-LTS releases.
	roundedJvmVersion := roundJVMVersionToNearestStableVersion(jvmVersion)

	return strconv.Itoa(roundedJvmVersion), nil
}

// roundJVMVersionToNearestStableVersion returns the oldest stable JVM version
// that is compatible with the given version. Java uses a time-based release
// schedule since Java 11. A new major version is released every 6 month and
// every 6th release is an LTS release. This means that a new LTS release gets
// published every 3rd year.  See
// https://www.baeldung.com/java-time-based-releases for more details.  This
// method rounds up non-LTS versions to the nearest LTS release. For example, a
// library that's published for Java 10 should be indexed with Java 11.
func roundJVMVersionToNearestStableVersion(javaVersion int) int {
	if javaVersion <= 8 {
		return 8
	}
	if javaVersion <= 11 {
		return 11
	}
	// TODO: bump this up to 17 once Java 17 LTS has been released, see https://github.com/sourcegraph/lsif-java/issues/263
	if javaVersion <= 16 {
		return 16
	}
	// Version from the future, do not round up to the next stable release.
	return javaVersion
}

type lsifJavaJSON struct {
	Kind         string   `json:"kind"`
	JVM          string   `json:"jvm"`
	Dependencies []string `json:"dependencies"`
}

// classFileMajorVersion returns the "major" version of the first `*.class` file
// inside the given jar file. For example, a jar file for a Java 8 library has
// the major version 52.
func classFileMajorVersion(byteCodeJarPath string) (string, error) {
	file, err := os.OpenFile(byteCodeJarPath, os.O_RDONLY, 0644)
	if err != nil {
		return "", err
	}
	defer file.Close()

	stat, err := os.Stat(byteCodeJarPath)
	if err != nil {
		return "", err
	}
	zipReader, err := zip.NewReader(file, stat.Size())
	if err != nil {
		return "", errors.Wrapf(err, "failed to read jar file %s", byteCodeJarPath)
	}

	for _, zipEntry := range zipReader.File {
		if !strings.HasSuffix(zipEntry.Name, ".class") {
			continue
		}
		version, err := classFileEntryMajorVersion(byteCodeJarPath, zipEntry)
		if err != nil {
			return "", nil
		}
		if version == "" {
			continue // Not a classfile
		}
		return version, nil
	}

	// We didn't find any `*.class` files so we can use any Java version.
	// Maven don't have to contain classfiles, some artifacts like
	// 'io.smallrye:smallrye-health-ui:3.1.1' only contain HTML/css/png/js
	// files.
	return "8", nil
}

func classFileEntryMajorVersion(byteCodeJarPath string, zipEntry *zip.File) (string, error) {
	classFileReader, err := zipEntry.Open()
	if err != nil {
		return "", err
	}

	magicBytes := make([]byte, 8)
	read, err := classFileReader.Read(magicBytes)
	defer classFileReader.Close()
	if err != nil {
		return "", err
	}
	if read != len(magicBytes) {
		return "", errors.Errorf("failed to read 8 bytes from file %s", byteCodeJarPath)
	}

	// The structure of `*.class` files is documented here
	// https://docs.oracle.com/javase/specs/jvms/se16/html/jvms-4.html#jvms-4.1 and also
	// https://en.wikipedia.org/wiki/Java_class_file#General_layout
	// - Bytes 0-4 must match 0xcafebabe.
	// - Bytes 4-5 represent the uint16 formatted "minor" version.
	// - Bytes 5-6 represent the uint16 formatted "major" version.
	// We're only interested in the major version.
	var cafebabe uint32
	var minor uint16
	var major uint16
	buf := bytes.NewReader(magicBytes)
	binary.Read(buf, binary.BigEndian, &cafebabe)
	if cafebabe != 0xcafebabe {
		return "", nil // Not a classfile
	}
	binary.Read(buf, binary.BigEndian, &minor)
	binary.Read(buf, binary.BigEndian, &major)
	return strconv.Itoa(int(major)), nil
}
