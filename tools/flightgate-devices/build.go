package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// buildItem freezes all three product repositories in fresh detached worktrees.
// It never reuses an existing artifact or overwrites the shared Android output.
func buildItem(args []string) error {
	_, source, _, _ := runtime.Caller(0)
	defaultTree := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	fs := flag.NewFlagSet("build-item", flag.ContinueOnError)
	commit := fs.String("commit", "", "connect commit to build (required)")
	program := fs.String("program", "", "source checkout directory; defaults to --tree")
	tree := fs.String("tree", defaultTree, "shared checkout directory (sdk, android, sibling modules and warp)")
	sdkBranch := fs.String("sdk-branch", "HEAD", "SDK revision to pair with the connect commit")
	androidCommit := fs.String("android-commit", "HEAD", "Android revision to build")
	patches := map[string]*string{}
	for _, name := range []string{"connect", "sdk", "android"} {
		patches[name] = fs.String(name+"-patch", "", "tracked "+name+" source patch applied to the detached revision and hashed in provenance")
	}
	diagSeconds := fs.Int("diag-seconds", 2, "transfer diagnostic interval baked into the AAR")
	memProfileRate := fs.Int("mem-profile-rate", 0, "heap sampling rate; nonzero produces a diagnostic-only artifact")
	suffix := fs.String("suffix", "", "optional build directory suffix")
	ndk := fs.String("ndk", "", "Android NDK directory; defaults to ANDROID_NDK_HOME or the app's pinned NDK")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *commit == "" {
		return errors.New("--commit is required")
	}
	if *diagSeconds <= 0 || *diagSeconds > 15 || *memProfileRate < 0 {
		return errors.New("--diag-seconds must be 1..15; --mem-profile-rate must be nonnegative")
	}
	if !regexp.MustCompile("^[A-Za-z0-9._-]*$").MatchString(*suffix) {
		return errors.New("--suffix must contain only letters, digits, dots, underscores or hyphens")
	}
	if *program == "" {
		*program = *tree
	}
	var err error
	if *program, err = filepath.Abs(*program); err != nil {
		return err
	}
	if *tree, err = filepath.Abs(*tree); err != nil {
		return err
	}
	revisions := map[string]string{}
	for name, ref := range map[string]string{"connect": *commit, "sdk": *sdkBranch, "android": *androidCommit} {
		revision, err := output("git", "-C", filepath.Join(*program, name), "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return fmt.Errorf("resolve %s revision: %w", name, err)
		}
		revisions[name] = revision
	}
	builds := filepath.Join(*program, "temp", "flightgate-builds")
	if err := os.MkdirAll(builds, 0o755); err != nil {
		return err
	}
	root, err := os.MkdirTemp(builds, revisions["connect"][:12]+*suffix+"-")
	if err != nil {
		return err
	}
	for _, name := range []string{"connect", "sdk", "android"} {
		if out, err := output("git", "-C", filepath.Join(*program, name), "worktree", "add", "--detach", filepath.Join(root, name), revisions[name]); err != nil {
			return fmt.Errorf("%s worktree: %v: %s", name, err, out)
		}
	}
	patchHashes := map[string]string{}
	for _, name := range []string{"connect", "sdk", "android"} {
		if *patches[name] == "" {
			continue
		}
		patch, err := filepath.Abs(*patches[name])
		if err != nil {
			return err
		}
		kept := filepath.Join(root, name+".patch")
		if err := copyArtifact(patch, kept); err != nil {
			return err
		}
		patchHashes[name], err = fileSHA256(kept)
		if err != nil {
			return err
		}
		if out, err := output("git", "-C", filepath.Join(root, name), "apply", "--check", kept); err != nil {
			return fmt.Errorf("%s source patch: %v: %s", name, err, out)
		}
		if out, err := output("git", "-C", filepath.Join(root, name), "apply", kept); err != nil {
			return fmt.Errorf("apply %s source patch: %v: %s", name, err, out)
		}
	}
	for _, sibling := range []string{"glog", "goidenticons", "proxy", "operator-proxy", "userwireguard", "sn", "warp"} {
		if err := os.Symlink(filepath.Join(*tree, sibling), filepath.Join(root, sibling)); err != nil {
			return err
		}
	}
	androidApp := filepath.Join(root, "android", "app")
	androidHome := os.Getenv("ANDROID_HOME")
	if androidHome == "" {
		androidHome = os.Getenv("ANDROID_SDK_ROOT")
	}
	if androidHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		androidHome = filepath.Join(home, "Library", "Android", "sdk")
	}
	if *ndk, err = resolveAndroidNDK(*ndk, os.Getenv("ANDROID_NDK_HOME"), androidHome, filepath.Join(androidApp, "app", "build.gradle")); err != nil {
		return err
	}
	buildID := "flightgate-c" + revisions["connect"][:12] + "-s" + revisions["sdk"][:12] + "-" + filepath.Base(root)
	sdkDir := filepath.Join(root, "sdk")
	godebugCmd := exec.Command("go", "list", "-f", "{{.DefaultGODEBUG}}", ".")
	godebugCmd.Dir = filepath.Join(sdkDir, "build")
	godebugOut, err := godebugCmd.Output()
	if err != nil {
		return fmt.Errorf("SDK Go compatibility defaults: %w", err)
	}
	godebug := strings.TrimSpace(string(godebugOut))
	if godebug != "" {
		godebug += ","
	}
	godebug += "memprofilerate=" + strconv.Itoa(*memProfileRate)
	ldflag := fmt.Sprintf("-X=runtime.godebugDefault=%s -X github.com/urnetwork/sdk.transferDiagLogSeconds=%d", godebug, *diagSeconds)
	fmt.Printf("build directory: %s\nbuild %s: connect %s, sdk %s, android %s\n", root, buildID, revisions["connect"], revisions["sdk"], revisions["android"])
	manifest := map[string]any{
		"build_id": buildID, "commits": revisions, "ndk": *ndk,
		"diag_seconds": *diagSeconds, "mem_profile_rate": *memProfileRate,
		"acceptance_eligible": *memProfileRate == 0, "runtime_ldflags": ldflag,
		"patch_sha256":                   patchHashes,
		"memory_profile":                 iosMemoryAuditProfile,
		"device_memory_target_bytes":     iosDeviceTargetBytes,
		"process_memory_limit_bytes":     iosProcessSoftLimitBytes,
		"process_transport_budget_bytes": iosCarrierRootBytes,
		"process_transport_max_count":    iosCarrierRootMaxCount,
	}
	if err := writeJson(filepath.Join(root, "build-manifest.json"), manifest); err != nil {
		return err
	}
	aar := exec.Command("make", "build_android", "MOBILE_RUNTIME_LDFLAG="+ldflag)
	aar.Dir = filepath.Join(sdkDir, "build")
	aar.Env = append(os.Environ(), "ANDROID_NDK_HOME="+*ndk, "ANDROID_HOME="+androidHome,
		"WARP_VERSION="+buildID, "URNETWORK_ANDROID_SDK_BUILD_OWNER="+buildID)
	if err := runBuildLogged(aar, filepath.Join(root, "build-aar.log")); err != nil {
		return err
	}
	aarPath := filepath.Join(sdkDir, "build", "android", "URnetworkSdk.aar")
	aarHash, err := fileSHA256(aarPath)
	if err != nil {
		return fmt.Errorf("AAR after build: %w", err)
	}
	apk := exec.Command("./gradlew", "--no-daemon", ":app:assembleGithubDebug", "-x", "buildSdk",
		"-PurnetworkAcceptanceBuildId="+buildID, "-PurnetworkMemoryProfileRateBytes="+strconv.Itoa(*memProfileRate),
		"-PurnetworkMemoryProfile="+iosMemoryAuditProfile)
	apk.Dir = androidApp
	apk.Env = append(os.Environ(), "BRINGYOUR_HOME="+root, "WARP_HOME="+*tree, "ANDROID_HOME="+androidHome)
	if err := runBuildLogged(apk, filepath.Join(root, "build-apk.log")); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(androidApp, "app", "build", "outputs", "apk", "github", "debug", "*arm64-v8a-debug.apk"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("expected one arm64 APK after build, found %d: %v", len(matches), err)
	}
	kept := filepath.Join(root, filepath.Base(matches[0]))
	if err := copyArtifact(matches[0], kept); err != nil {
		return err
	}
	apkHash, err := fileSHA256(kept)
	if err != nil {
		return err
	}
	manifest["aar_sha256"], manifest["apk_sha256"], manifest["apk"] = aarHash, apkHash, kept
	if err := writeJson(filepath.Join(root, "build-manifest.json"), manifest); err != nil {
		return err
	}
	fmt.Printf("AAR SHA-256: %s\nAPK SHA-256: %s\n%s\n", aarHash, apkHash, kept)
	return nil
}

func resolveAndroidNDK(explicit, env, androidHome, gradlePath string) (string, error) {
	path := explicit
	if path == "" {
		path = env
	}
	if path == "" {
		gradle, err := os.ReadFile(gradlePath)
		if err != nil {
			return "", err
		}
		match := regexp.MustCompile("ndkVersion\\s*=\\s*['\"]([^'\"]+)['\"]").FindSubmatch(gradle)
		if len(match) != 2 {
			return "", errors.New("cannot determine pinned Android NDK; set --ndk or ANDROID_NDK_HOME")
		}
		path = filepath.Join(androidHome, "ndk", string(match[1]))
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, "source.properties")); err != nil {
		return "", fmt.Errorf("Android NDK %s unavailable: %w", path, err)
	}
	return path, nil
}

func runBuildLogged(cmd *exec.Cmd, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = file, file
	err = cmd.Run()
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("build failed (%v), see %s", err, path)
	}
	return closeErr
}

func copyArtifact(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
