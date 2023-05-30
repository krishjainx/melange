// Copyright 2022 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"chainguard.dev/apko/pkg/log"
	sign "github.com/chainguard-dev/go-apk/pkg/signature"
	"github.com/chainguard-dev/go-apk/pkg/tarball"
	"github.com/psanford/memfs"
)

type PackageContext struct {
	Context       *Context
	Origin        *Package
	PackageName   string
	OriginName    string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Logger        log.Logger
	Dependencies  Dependencies
	Arch          string
	Options       PackageOption
	Scriptlets    Scriptlets
	Description   string
	URL           string
	Commit        string
}

func (pkg *Package) Emit(sigh context.Context, ctx *PipelineContext) error {
	fakesp := Subpackage{
		Name:         pkg.Name,
		Dependencies: pkg.Dependencies,
		Options:      pkg.Options,
		Scriptlets:   pkg.Scriptlets,
		Description:  pkg.Description,
		URL:          pkg.URL,
		Commit:       pkg.Commit,
	}
	return fakesp.Emit(sigh, ctx)
}

func (spkg *Subpackage) Emit(sigh context.Context, ctx *PipelineContext) error {
	pc := PackageContext{
		Context:      ctx.Context,
		Origin:       &ctx.Context.Configuration.Package,
		PackageName:  spkg.Name,
		OriginName:   spkg.Name,
		OutDir:       filepath.Join(ctx.Context.OutDir, ctx.Context.Arch.ToAPK()),
		Logger:       ctx.Context.Logger,
		Dependencies: spkg.Dependencies,
		Arch:         ctx.Context.Arch.ToAPK(),
		Options:      spkg.Options,
		Scriptlets:   spkg.Scriptlets,
		Description:  spkg.Description,
		URL:          spkg.URL,
		Commit:       spkg.Commit,
	}

	if !ctx.Context.StripOriginName {
		pc.OriginName = pc.Origin.Name
	}

	return pc.EmitPackage(sigh)
}

// AppendBuildLog will create or append a list of packages that were built by melange build
func (pc *PackageContext) AppendBuildLog(dir string) error {
	if !pc.Context.CreateBuildLog {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(dir, "packages.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// separate with pipe so it is easy to parse
	_, err = f.WriteString(fmt.Sprintf("%s|%s|%s|%s-r%d\n", pc.Arch, pc.OriginName, pc.PackageName, pc.Origin.Version, pc.Origin.Epoch))
	return err
}

func (pc *PackageContext) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageContext) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageContext) WorkspaceSubdir() string {
	return filepath.Join(pc.Context.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = {{.Arch}}
size = {{.InstalledSize}}
origin = {{.OriginName}}
pkgdesc = {{.Description}}
url = {{.URL}}
commit = {{.Commit}}
{{- if ne .Context.SourceDateEpoch.Unix 0 }}
builddate = {{ .Context.SourceDateEpoch.Unix }}
{{- end}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Provides }}
provides = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Replaces }}
replaces = {{ $dep }}
{{- end }}
{{- if .Dependencies.ProviderPriority }}
provider_priority = {{ .Dependencies.ProviderPriority }}
{{- end }}
{{- if .Scriptlets.Trigger.Paths }}
triggers = {{ range $item := .Scriptlets.Trigger.Paths }}{{ $item }} {{ end }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageContext) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageContext) generateControlSection(ctx context.Context, digest hash.Hash, w io.WriteSeeker) (hash.Hash, error) {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return digest, fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return digest, fmt.Errorf("unable to process control template: %w", err)
	}

	fsys := memfs.New()
	if err := fsys.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return digest, fmt.Errorf("unable to build control FS: %w", err)
	}

	if pc.Scriptlets.Trigger.Script != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".trigger", []byte(pc.Scriptlets.Trigger.Script), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-install", []byte(pc.Scriptlets.PreInstall), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-install", []byte(pc.Scriptlets.PostInstall), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-deinstall", []byte(pc.Scriptlets.PreDeinstall), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-deinstall", []byte(pc.Scriptlets.PostDeinstall), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-upgrade", []byte(pc.Scriptlets.PreUpgrade), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-upgrade", []byte(pc.Scriptlets.PostUpgrade), 0755); err != nil {
			return digest, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	mw := io.MultiWriter(digest, w)
	if err := tarctx.WriteTargz(ctx, mw, fsys); err != nil {
		return digest, fmt.Errorf("unable to write control tarball: %w", err)
	}

	controlHash := hex.EncodeToString(digest.Sum(nil))
	pc.Logger.Printf("  control.tar.gz digest: %s", controlHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return digest, fmt.Errorf("unable to rewind control tarball: %w", err)
	}

	return digest, nil
}

func (pc *PackageContext) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Context.SigningKey))
}

type DependencyGenerator func(*PackageContext, *Dependencies) error

func dedup(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))

	var prev string
	for _, cur := range in {
		if cur == prev {
			continue
		}
		out = append(out, cur)
		prev = cur
	}

	return out
}

func allowedPrefix(path string, prefixes []string) bool {
	for _, pfx := range prefixes {
		if strings.HasPrefix(path, pfx) {
			return true
		}
	}

	return false
}

var cmdPrefixes = []string{"bin", "sbin", "usr/bin", "usr/sbin"}

func generateCmdProviders(pc *PackageContext, generated *Dependencies) error {
	if pc.Options.NoCommands {
		return nil
	}

	pc.Logger.Printf("scanning for commands...")

	fsys := readlinkFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			if allowedPrefix(path, cmdPrefixes) {
				basename := filepath.Base(path)
				generated.Provides = append(generated.Provides, fmt.Sprintf("cmd:%s=%s-r%d", basename, pc.Origin.Version, pc.Origin.Epoch))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// findInterpreter looks for the PT_INTERP header and extracts the interpreter so that it
// may be used as a dependency.
func findInterpreter(bin *elf.File) (string, error) {
	for _, prog := range bin.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}

		reader := prog.Open()
		interpBuf, err := io.ReadAll(reader)
		if err != nil {
			return "", err
		}

		interpBuf = bytes.Trim(interpBuf, "\x00")
		return string(interpBuf), nil
	}

	return "", nil
}

// dereferenceCrossPackageSymlink attempts to dereference a symlink across multiple package
// directories.
func (pc *PackageContext) dereferenceCrossPackageSymlink(path string) (string, error) {
	libDirs := []string{"lib", "usr/lib", "lib64", "usr/lib64"}
	targetPackageNames := []string{pc.PackageName, pc.Context.Configuration.Package.Name}
	realPath, err := os.Readlink(filepath.Join(pc.WorkspaceSubdir(), path))
	if err != nil {
		return "", err
	}

	realPath = filepath.Base(realPath)

	for _, subPkg := range pc.Context.Configuration.Subpackages {
		targetPackageNames = append(targetPackageNames, subPkg.Name)
	}

	for _, pkgName := range targetPackageNames {
		basePath := filepath.Join(pc.Context.WorkspaceDir, "melange-out", pkgName)

		for _, libDir := range libDirs {
			testPath := filepath.Join(basePath, libDir, realPath)

			if _, err := os.Stat(testPath); err == nil {
				return testPath, nil
			}
		}
	}

	return "", nil
}

func generateSharedObjectNameDeps(pc *PackageContext, generated *Dependencies) error {
	pc.Logger.Printf("scanning for shared object dependencies...")

	depends := map[string][]string{}

	fsys := readlinkFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()

		// If it is a symlink, lets check and see if it is a library SONAME.
		if mode.Type()&fs.ModeSymlink == fs.ModeSymlink {
			if !strings.Contains(path, ".so") {
				return nil
			}

			realPath, err := pc.dereferenceCrossPackageSymlink(path)
			if err != nil {
				return nil
			}

			if realPath != "" {
				ef, err := elf.Open(realPath)
				if err != nil {
					return nil
				}
				defer ef.Close()

				sonames, err := ef.DynString(elf.DT_SONAME)
				// most likely SONAME is not set on this object
				if err != nil {
					pc.Logger.Printf("WARNING: library %s lacks SONAME", path)
					return nil
				}

				for _, soname := range sonames {
					generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", soname))
				}
			}

			return nil
		}

		// If it is not a regular file, we are finished processing it.
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			basename := filepath.Base(path)

			// most likely a shell script instead of an ELF, so treat any
			// error as non-fatal.
			// TODO(kaniini): use DirFS for this
			ef, err := elf.Open(filepath.Join(pc.WorkspaceSubdir(), path))
			if err != nil {
				return nil
			}
			defer ef.Close()

			interp, err := findInterpreter(ef)
			if err != nil {
				return err
			}
			if interp != "" && !pc.Options.NoDepends {
				pc.Logger.Printf("interpreter for %s => %s", basename, interp)

				// musl interpreter is a symlink back to itself, so we want to use the non-symlink name as
				// the dependency.
				interpName := fmt.Sprintf("so:%s", filepath.Base(interp))
				interpName = strings.ReplaceAll(interpName, "so:ld-musl", "so:libc.musl")
				generated.Runtime = append(generated.Runtime, interpName)
			}

			libs, err := ef.ImportedLibraries()
			if err != nil {
				pc.Logger.Printf("WTF: ImportedLibraries() returned error: %v", err)
				return nil
			}

			if !pc.Options.NoDepends {
				for _, lib := range libs {
					if strings.Contains(lib, ".so.") {
						generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", lib))
						depends[lib] = append(depends[lib], path)
					}
				}
			}

			// An executable program should never have a SONAME, but apparently binaries built
			// with some versions of jlink do.  Thus, if an interpreter is set (meaning it is an
			// executable program), we do not scan the object for SONAMEs.
			if !pc.Options.NoProvides && interp == "" {
				sonames, err := ef.DynString(elf.DT_SONAME)
				// most likely SONAME is not set on this object
				if err != nil {
					pc.Logger.Printf("WARNING: library %s lacks SONAME", path)
					return nil
				}

				for _, soname := range sonames {
					parts := strings.Split(soname, ".so.")

					var libver string
					if len(parts) > 1 {
						libver = parts[1]
					} else {
						libver = "0"
					}

					generated.Provides = append(generated.Provides, fmt.Sprintf("so:%s=%s", soname, libver))
				}
			}
		}

		return nil
	}); err != nil {
		return err
	}

	if pc.Context.DependencyLog != "" {
		pc.Logger.Printf("writing dependency log")

		logFile, err := os.Create(fmt.Sprintf("%s.%s", pc.Context.DependencyLog, pc.Arch))
		if err != nil {
			pc.Logger.Printf("WARNING: Unable to open dependency log: %v", err)
		}
		defer logFile.Close()

		je := json.NewEncoder(logFile)
		if err := je.Encode(depends); err != nil {
			return err
		}
	}

	return nil
}

func (dep *Dependencies) Summarize(logger log.Logger) {
	if len(dep.Runtime) > 0 {
		logger.Printf("  runtime:")

		for _, dep := range dep.Runtime {
			logger.Printf("    %s", dep)
		}
	}

	if len(dep.Provides) > 0 {
		logger.Printf("  provides:")

		for _, dep := range dep.Provides {
			logger.Printf("    %s", dep)
		}
	}
}

// removeSelfProvidedDeps removes dependencies which are provided by the package itself.
func removeSelfProvidedDeps(runtimeDeps, providedDeps []string) []string {
	providedDepsMap := map[string]bool{}

	for _, versionedDep := range providedDeps {
		dep := strings.Split(versionedDep, "=")[0]
		providedDepsMap[dep] = true
	}

	newRuntimeDeps := []string{}
	for _, dep := range runtimeDeps {
		_, ok := providedDepsMap[dep]
		if ok {
			continue
		}

		newRuntimeDeps = append(newRuntimeDeps, dep)
	}

	return newRuntimeDeps
}

func (pc *PackageContext) GenerateDependencies() error {
	generated := Dependencies{}
	generators := []DependencyGenerator{
		generateSharedObjectNameDeps,
		generateCmdProviders,
	}

	for _, gen := range generators {
		if err := gen(pc, &generated); err != nil {
			return err
		}
	}

	newruntime := append(pc.Dependencies.Runtime, generated.Runtime...)
	pc.Dependencies.Runtime = dedup(newruntime)

	newprovides := append(pc.Dependencies.Provides, generated.Provides...)
	pc.Dependencies.Provides = dedup(newprovides)

	pc.Dependencies.Runtime = removeSelfProvidedDeps(pc.Dependencies.Runtime, pc.Dependencies.Provides)

	pc.Dependencies.Summarize(pc.Logger)

	return nil
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageContext) calculateInstalledSize(fsys fs.FS) error {
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	return nil
}

func (pc *PackageContext) emitDataSection(ctx context.Context, fsys fs.FS, w io.WriteSeeker) error {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	digest := sha256.New()
	mw := io.MultiWriter(digest, w)
	if err := tarctx.WriteTargz(ctx, mw, fsys); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	pc.DataHash = hex.EncodeToString(digest.Sum(nil))
	pc.Logger.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	return nil
}

func (pc *PackageContext) emitNormalSignatureSection(ctx context.Context, h hash.Hash, w io.WriteSeeker) error {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	fsys := memfs.New()
	sigbuf, err := sign.RSASignSHA1Digest(h.Sum(nil), pc.Context.SigningKey, pc.Context.SigningPassphrase)
	if err != nil {
		return fmt.Errorf("unable to generate signature: %w", err)
	}

	if err := fsys.WriteFile(pc.SignatureName(), sigbuf, 0644); err != nil {
		return fmt.Errorf("unable to build signature FS: %w", err)
	}

	if err := tarctx.WriteTargz(ctx, w, fsys); err != nil {
		return fmt.Errorf("unable to write signature tarball: %w", err)
	}

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind signature tarball: %w", err)
	}

	return nil
}

func (pc *PackageContext) wantSignature() bool {
	return pc.Context.SigningKey != ""
}

func (pc *PackageContext) EmitPackage(ctx context.Context) error {
	err := os.MkdirAll(pc.WorkspaceSubdir(), 0o755)
	if err != nil {
		return fmt.Errorf("unable to ensure workspace exists: %w", err)
	}

	pc.Logger.Printf("generating package %s", pc.Identity())

	// filesystem for the data package
	fsys := readlinkFS(pc.WorkspaceSubdir())

	// generate so:/cmd: virtuals for the filesystem
	if err := pc.GenerateDependencies(); err != nil {
		return fmt.Errorf("unable to build final dependencies set: %w", err)
	}

	// walk the filesystem to calculate the installed-size
	if err := pc.calculateInstalledSize(fsys); err != nil {
		return err
	}

	pc.Logger.Printf("  installed-size: %d", pc.InstalledSize)

	// prepare data.tar.gz
	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()
	defer os.Remove(dataTarGz.Name())

	if err := pc.emitDataSection(ctx, fsys, dataTarGz); err != nil {
		return err
	}

	// prepare control.tar.gz
	controlTarGz, err := os.CreateTemp("", "melange-control-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer controlTarGz.Close()
	defer os.Remove(controlTarGz.Name())

	var controlDigest hash.Hash

	// APKv2 style signature is a SHA-1 hash on the control digest,
	// APKv2+Fulcio style signature is an SHA-256 hash on the control
	// digest.
	controlDigest = sha256.New()

	// Key-based signature (normal), use SHA-1
	if pc.Context.SigningKey != "" {
		controlDigest = sha1.New()
	}

	finalDigest, err := pc.generateControlSection(ctx, controlDigest, controlTarGz)
	if err != nil {
		return err
	}

	combinedParts := []io.Reader{controlTarGz, dataTarGz}

	if pc.wantSignature() {
		signatureTarGz, err := os.CreateTemp("", "melange-signature-*.tar.gz")
		if err != nil {
			return fmt.Errorf("unable to open temporary file for writing: %w", err)
		}
		defer signatureTarGz.Close()
		defer os.Remove(signatureTarGz.Name())

		// TODO(kaniini): Emit fulcio signature if signing key not configured.
		if err := pc.emitNormalSignatureSection(ctx, finalDigest, signatureTarGz); err != nil {
			return err
		}

		combinedParts = append([]io.Reader{signatureTarGz}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	pc.Logger.Printf("wrote %s", outFile.Name())

	// add the package to the build log if requested
	if err := pc.AppendBuildLog(""); err != nil {
		pc.Logger.Warnf("unable to append package log: %s", err)
	}

	return nil
}
