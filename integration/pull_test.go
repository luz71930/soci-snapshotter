/*
   Copyright The Soci Snapshotter Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shell "github.com/awslabs/soci-snapshotter/util/dockershell"
	"github.com/awslabs/soci-snapshotter/util/dockershell/compose"
	"github.com/awslabs/soci-snapshotter/util/testutil"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/xid"
)

const (
	defaultContainerdConfigPath  = "/etc/containerd/config.toml"
	defaultSnapshotterConfigPath = "/etc/soci-snapshotter-grpc/config.toml"
	alpineImage                  = "alpine:latest"
	ubuntuImage                  = "ubuntu:latest"
	dockerLibrary                = "public.ecr.aws/docker/library/"
)

const proxySnapshotterConfig = `
[proxy_plugins]
  [proxy_plugins.soci]
    type = "snapshot"
    address = "/run/soci-snapshotter-grpc/soci-snapshotter-grpc.sock"
`

// TestSnapshotterStartup tests to run containerd + snapshotter and check plugin is
// recognized by containerd
func TestSnapshotterStartup(t *testing.T) {
	t.Parallel()
	sh, done := newSnapshotterBaseShell(t)
	defer done()
	rebootContainerd(t, sh, "", "")
	found := false
	err := sh.ForEach(shell.C("ctr", "plugin", "ls"), func(l string) bool {
		info := strings.Fields(l)
		if len(info) < 4 {
			t.Fatalf("malformed plugin info: %v", info)
		}
		if info[0] == "io.containerd.snapshotter.v1" && info[1] == "soci" && info[3] == "ok" {
			found = true
			return false
		}
		return true
	})
	if err != nil || !found {
		t.Fatalf("failed to get soci snapshotter status using ctr plugin ls: %v", err)
	}
}

// TestOptimizeConsistentSociArtifact tests if the Soci artifact is produced consistently across runs.
// This test does the following:
// 1. Generate Soci artifact
// 2. Copy the local content store to another folder
// 3. Generate Soci artifact for the same image again
// 4. Do the comparison of the Soci artifact blobs
// 5. Clean up the local content store folder and the folder used for comparison
// Due to the reason that this test will be doing manipulations with local content store folder,
// it should be never run in parallel with the other tests.
func TestOptimizeConsistentSociArtifact(t *testing.T) {
	var (
		registryHost = "registry-" + xid.New().String() + ".test"
		registryUser = "dummyuser"
		registryPass = "dummypass"
	)
	dockerhub := func(name string) imageInfo {
		return imageInfo{dockerLibrary + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}

	// Setup environment
	sh, _, done := newShellWithRegistry(t, registryHost, registryUser, registryPass)
	defer done()

	tests := []struct {
		name           string
		containerImage string
	}{
		{
			name:           "soci artifact is consistently built for ubuntu",
			containerImage: "ubuntu:latest",
		},
		/*
			The following test is disabled for now, since it randomly fails due to
			the way gob encodes FileMetadata.Xattrs.
			TODO(rdpsin): Re-enable this test once gob is no longer used to encode FileMetadata.
			{
				name:           "soci artifact is consistently built for nginx",
				containerImage: "nginx:latest",
			},
		*/
		{
			name:           "soci artifact is consistently built for drupal",
			containerImage: "alpine:latest",
		},
	}
	const blobStorePath = "/var/lib/soci-snapshotter-grpc/content/blobs/sha256"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rebootContainerd(t, sh, "", "")
			copyImage(sh, dockerhub(tt.containerImage), mirror(tt.containerImage))
			// optimize for the first time
			sh.
				X("rm", "-rf", blobStorePath)
			optimizeImage(sh, mirror(tt.containerImage))
			// move the artifact to a folder
			sh.
				X("rm", "-rf", "copy").
				X("mkdir", "copy").
				X("cp", "-r", blobStorePath, "copy") // move the contents of soci dir to another folder

			// optimize for the second time
			optimizeImage(sh, mirror(tt.containerImage))

			currContent := sh.O("ls", blobStorePath)
			prevContent := sh.O("ls", "copy/sha256")
			if !bytes.Equal(currContent, prevContent) {
				t.Fatalf("local content store: previously generated artifact is different")
			}

			fileNames := strings.Fields(string(currContent))
			for _, fn := range fileNames {
				if fn == "artifacts.db" {
					// skipping artifacts.db, since this is bbolt file and we have no control over its internals
					continue
				}
				out := sh.OLog("cmp", filepath.Join("soci", fn), filepath.Join("copy", "soci", fn))
				if string(out) != "" {
					t.Fatalf("the artifact is different: %v", string(out))
				}
			}

			sh.X("rm", "-rf", blobStorePath).X("rm", "-rf", "copy")
		})
	}
}

func TestLazyPullWithSparseIndex(t *testing.T) {
	t.Parallel()
	var (
		registryHost  = "registry-" + xid.New().String() + ".test"
		registryUser  = "dummyuser"
		registryPass  = "dummypass"
		registryCreds = func() string { return registryUser + ":" + registryPass }
	)
	dockerhub := func(name string) imageInfo {
		return imageInfo{dockerLibrary + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}
	// Prepare config for containerd and snapshotter
	getContainerdConfigYaml := func(disableVerification bool) []byte {
		additionalConfig := ""
		if !isTestingBuiltinSnapshotter() {
			additionalConfig = proxySnapshotterConfig
		}
		return []byte(testutil.ApplyTextTemplate(t, `
version = 2

[plugins."io.containerd.snapshotter.v1.soci"]
root_path = "/var/lib/soci-snapshotter-grpc/"
disable_verification = {{.DisableVerification}}

[plugins."io.containerd.snapshotter.v1.soci".blob]
check_always = true

[debug]
format = "json"
level = "debug"

{{.AdditionalConfig}}
`, struct {
			DisableVerification bool
			AdditionalConfig    string
		}{
			DisableVerification: disableVerification,
			AdditionalConfig:    additionalConfig,
		}))
	}
	getSnapshotterConfigYaml := func(disableVerification bool) []byte {
		return []byte(fmt.Sprintf("disable_verification = %v", disableVerification))
	}

	// Setup environment
	sh, _, done := newShellWithRegistry(t, registryHost, registryUser, registryPass)
	defer done()
	if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, getContainerdConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultContainerdConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, defaultSnapshotterConfigPath, getSnapshotterConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultSnapshotterConfigPath, err)
	}

	const imageName = "rethinkdb@sha256:4452aadba3e99771ff3559735dab16279c5a352359d79f38737c6fdca941c6e5"
	const imageManifestDigest = "sha256:4452aadba3e99771ff3559735dab16279c5a352359d79f38737c6fdca941c6e5"
	const minLayerSize = 10000000

	rebootContainerd(t, sh, "", "")
	copyImage(sh, dockerhub(imageName), mirror(imageName))
	indexDigest := buildSparseIndex(sh, mirror(imageName), minLayerSize)

	fromNormalSnapshotter := func(image string) tarPipeExporter {
		return func(tarExportArgs ...string) {
			rebootContainerd(t, sh, "", "")
			sh.X("ctr", "i", "pull", "--user", registryCreds(), image)
			sh.Pipe(nil, shell.C("ctr", "run", "--rm", image, "test", "tar", "-c", "/usr"), tarExportArgs)
		}
	}
	export := func(sh *shell.Shell, image string, tarExportArgs []string) {
		sh.X("soci", "image", "rpull", "--user", registryCreds(), "--soci-index-digest", indexDigest, image)
		sh.Pipe(nil, shell.C("soci", "run", "--rm", "--snapshotter=soci", image, "test", "tar", "-c", "/usr"), tarExportArgs)
	}

	imageManifestJSON := fetchContentByDigest(sh, imageManifestDigest)
	imageManifest := new(ocispec.Manifest)
	if err := json.Unmarshal(imageManifestJSON, imageManifest); err != nil {
		t.Fatalf("cannot unmarshal index manifest: %v", err)
	}

	layersToDownload := make([]ocispec.Descriptor, 0)
	for _, layerBlob := range imageManifest.Layers {
		if layerBlob.Size < minLayerSize {
			layersToDownload = append(layersToDownload, layerBlob)
		}
	}
	remoteSnapshotsExpectedCount := len(imageManifest.Layers) - len(layersToDownload)
	checkFuseMounts := func(t *testing.T, sh *shell.Shell, remoteSnapshotsExpectedCount int) {
		mounts := string(sh.O("mount"))
		remoteSnapshotsActualCount := strings.Count(mounts, "fuse.rawBridge")
		if remoteSnapshotsExpectedCount != remoteSnapshotsActualCount {
			t.Fatalf("incorrect number of remote snapshots; expected=%d, actual=%d",
				remoteSnapshotsExpectedCount, remoteSnapshotsActualCount)
		}
	}

	checkLayersInSnapshottersContentStore := func(t *testing.T, sh *shell.Shell, layers []ocispec.Descriptor) {
		for _, layer := range layers {
			layerPath := filepath.Join(blobStorePath, trimSha256Prefix(layer.Digest.String()))
			existenceResult := strings.TrimSuffix(string(sh.O("ls", layerPath)), "\n")
			if layerPath != existenceResult {
				t.Fatalf("layer file %s was not found in snapshotter's local content store, the result of ls=%s", layerPath, existenceResult)
			}
		}
	}

	tests := []struct {
		name string
		want tarPipeExporter
		test tarPipeExporter
	}{
		{
			name: "Soci",
			want: fromNormalSnapshotter(mirror(imageName).ref),
			test: func(tarExportArgs ...string) {
				image := mirror(imageName).ref
				rebootContainerd(t, sh, "", "")
				buildSparseIndex(sh, mirror(imageName), minLayerSize)
				sh.X("ctr", "i", "rm", imageName)
				export(sh, image, tarExportArgs)
				checkFuseMounts(t, sh, remoteSnapshotsExpectedCount)
				checkLayersInSnapshottersContentStore(t, sh, layersToDownload)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testSameTarContents(t, sh, tt.want, tt.test)
		})
	}
}

// TestLazyPull tests if lazy pulling works.
func TestLazyPull(t *testing.T) {
	t.Parallel()
	var (
		registryHost  = "registry-" + xid.New().String() + ".test"
		registryUser  = "dummyuser"
		registryPass  = "dummypass"
		registryCreds = func() string { return registryUser + ":" + registryPass }
	)
	dockerhub := func(name string) imageInfo {
		return imageInfo{dockerLibrary + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}
	// Prepare config for containerd and snapshotter
	getContainerdConfigYaml := func(disableVerification bool) []byte {
		additionalConfig := ""
		if !isTestingBuiltinSnapshotter() {
			additionalConfig = proxySnapshotterConfig
		}
		return []byte(testutil.ApplyTextTemplate(t, `
version = 2

[plugins."io.containerd.snapshotter.v1.soci"]
root_path = "/var/lib/soci-snapshotter-grpc/"
disable_verification = {{.DisableVerification}}

[plugins."io.containerd.snapshotter.v1.soci".blob]
check_always = true

[debug]
format = "json"
level = "debug"

{{.AdditionalConfig}}
`, struct {
			DisableVerification bool
			AdditionalConfig    string
		}{
			DisableVerification: disableVerification,
			AdditionalConfig:    additionalConfig,
		}))
	}
	getSnapshotterConfigYaml := func(disableVerification bool) []byte {
		return []byte(fmt.Sprintf("disable_verification = %v", disableVerification))
	}

	// Setup environment
	sh, _, done := newShellWithRegistry(t, registryHost, registryUser, registryPass)
	defer done()
	if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, getContainerdConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultContainerdConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, defaultSnapshotterConfigPath, getSnapshotterConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultSnapshotterConfigPath, err)
	}

	optimizedImageName := alpineImage
	nonOptimizedImageName := ubuntuImage

	// Mirror images
	rebootContainerd(t, sh, "", "")
	copyImage(sh, dockerhub(optimizedImageName), mirror(optimizedImageName))
	copyImage(sh, dockerhub(nonOptimizedImageName), mirror(nonOptimizedImageName))
	indexDigest := optimizeImage(sh, mirror(optimizedImageName))

	// Test if contents are pulled
	fromNormalSnapshotter := func(image string) tarPipeExporter {
		return func(tarExportArgs ...string) {
			rebootContainerd(t, sh, "", "")
			sh.X("ctr", "i", "pull", "--user", registryCreds(), image)
			sh.Pipe(nil, shell.C("ctr", "run", "--rm", image, "test", "tar", "-c", "/usr"), tarExportArgs)
		}
	}
	export := func(sh *shell.Shell, image string, tarExportArgs []string) {
		sh.X("soci", "image", "rpull", "--user", registryCreds(), "--soci-index-digest", indexDigest, image)
		sh.Pipe(nil, shell.C("soci", "run", "--rm", "--snapshotter=soci", image, "test", "tar", "-c", "/usr"), tarExportArgs)
	}

	// NOTE: these tests must be executed sequentially.
	tests := []struct {
		name string
		want tarPipeExporter
		test tarPipeExporter
	}{
		{
			name: "normal",
			want: fromNormalSnapshotter(mirror(nonOptimizedImageName).ref),
			test: func(tarExportArgs ...string) {
				image := mirror(nonOptimizedImageName).ref
				rebootContainerd(t, sh, "", "")
				export(sh, image, tarExportArgs)
			},
		},
		{
			name: "Soci",
			want: fromNormalSnapshotter(mirror(optimizedImageName).ref),
			test: func(tarExportArgs ...string) {
				image := mirror(optimizedImageName).ref
				m := rebootContainerd(t, sh, "", "")
				optimizeImage(sh, mirror(optimizedImageName))
				sh.X("ctr", "i", "rm", optimizedImageName)
				export(sh, image, tarExportArgs)
				m.CheckAllRemoteSnapshots(t)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testSameTarContents(t, sh, tt.want, tt.test)
		})
	}
}

// TestMirror tests if mirror & refreshing functionalities of snapshotter work
func TestMirror(t *testing.T) {
	t.Parallel()
	var (
		reporter        = testutil.NewTestingReporter(t)
		pRoot           = testutil.GetProjectRoot(t)
		caCertDir       = "/usr/local/share/ca-certificates"
		registryHost    = "registry-" + xid.New().String() + ".test"
		registryAltHost = "registry-alt-" + xid.New().String() + ".test"
		registryUser    = "dummyuser"
		registryPass    = "dummypass"
		registryCreds   = func() string { return registryUser + ":" + registryPass }
		serviceName     = "testing_mirror"
	)
	dockerhub := func(name string) imageInfo {
		return imageInfo{dockerLibrary + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}
	mirror2 := func(name string) imageInfo {
		return imageInfo{registryAltHost + ":5000/" + name, "", true}
	}

	// Setup dummy creds for test
	crt, key, err := generateRegistrySelfSignedCert(registryHost)
	if err != nil {
		t.Fatalf("failed to generate cert: %v", err)
	}
	htpasswd, err := generateBasicHtpasswd(registryUser, registryPass)
	if err != nil {
		t.Fatalf("failed to generate htpasswd: %v", err)
	}

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "domain.key"), key, 0666); err != nil {
		t.Fatalf("failed to prepare key file")
	}
	if err := os.WriteFile(filepath.Join(authDir, "domain.crt"), crt, 0666); err != nil {
		t.Fatalf("failed to prepare crt file")
	}
	if err := os.WriteFile(filepath.Join(authDir, "htpasswd"), htpasswd, 0666); err != nil {
		t.Fatalf("failed to prepare htpasswd file")
	}

	targetStage := "containerd-snapshotter-base"

	// Run testing environment on docker compose
	c, err := compose.New(testutil.ApplyTextTemplate(t, `
version: "3.7"
services:
  {{.ServiceName}}:
    build:
      context: {{.ImageContextDir}}
      target: {{.TargetStage}}
    privileged: true
    init: true
    entrypoint: [ "sleep", "infinity" ]
    environment:
    - NO_PROXY=127.0.0.1,localhost,{{.RegistryHost}}:443
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - /dev/fuse:/dev/fuse
    - "lazy-containerd-data:/var/lib/containerd"
    - "lazy-soci-snapshotter-grpc-data:/var/lib/soci-snapshotter-grpc"
  registry:
    image: ghcr.io/oras-project/registry:v1.0.0-rc
    container_name: {{.RegistryHost}}
    environment:
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/domain.key
    - REGISTRY_HTTP_ADDR={{.RegistryHost}}:443
    volumes:
    - {{.AuthDir}}:/auth:ro
  registry-alt:
    image: registry:2
    container_name: {{.RegistryAltHost}}
volumes:
  lazy-containerd-data:
  lazy-soci-snapshotter-grpc-data:
`, struct {
		TargetStage     string
		ServiceName     string
		ImageContextDir string
		RegistryHost    string
		RegistryAltHost string
		AuthDir         string
	}{
		TargetStage:     targetStage,
		ServiceName:     serviceName,
		ImageContextDir: pRoot,
		RegistryHost:    registryHost,
		RegistryAltHost: registryAltHost,
		AuthDir:         authDir,
	}),
		compose.WithBuildArgs(getBuildArgsFromEnv(t)...),
		compose.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to prepare compose: %v", err)
	}
	defer c.Cleanup()
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v: %v", serviceName, err)
	}
	sh := shell.New(de, reporter)

	// Initialize config files for containerd and snapshotter
	additionalConfig := ""
	if !isTestingBuiltinSnapshotter() {
		additionalConfig = proxySnapshotterConfig
	}
	containerdConfigYaml := testutil.ApplyTextTemplate(t, `
version = 2

[plugins."io.containerd.snapshotter.v1.soci"]
root_path = "/var/lib/soci-snapshotter-grpc/"

[plugins."io.containerd.snapshotter.v1.soci".blob]
check_always = true

[[plugins."io.containerd.snapshotter.v1.soci".resolver.host."{{.RegistryHost}}".mirrors]]
host = "{{.RegistryAltHost}}:5000"
insecure = true

{{.AdditionalConfig}}
`, struct {
		RegistryHost     string
		RegistryAltHost  string
		AdditionalConfig string
	}{
		RegistryHost:     registryHost,
		RegistryAltHost:  registryAltHost,
		AdditionalConfig: additionalConfig,
	})
	snapshotterConfigYaml := testutil.ApplyTextTemplate(t, `
[blob]
check_always = true

[[resolver.host."{{.RegistryHost}}".mirrors]]
host = "{{.RegistryAltHost}}:5000"
insecure = true
`, struct {
		RegistryHost    string
		RegistryAltHost string
	}{
		RegistryHost:    registryHost,
		RegistryAltHost: registryAltHost,
	})

	// Setup environment
	if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, []byte(containerdConfigYaml), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultContainerdConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, defaultSnapshotterConfigPath, []byte(snapshotterConfigYaml), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultSnapshotterConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, filepath.Join(caCertDir, "domain.crt"), crt, 0600); err != nil {
		t.Fatalf("failed to write %v: %v", caCertDir, err)
	}
	sh.
		X("apt-get", "--no-install-recommends", "install", "-y", "iptables").
		X("update-ca-certificates").
		Retry(100, "nerdctl", "login", "-u", registryUser, "-p", registryPass, registryHost)

	imageName := alpineImage
	// Mirror images
	rebootContainerd(t, sh, "", "")
	copyImage(sh, dockerhub(imageName), mirror(imageName))
	copyImage(sh, mirror(imageName), mirror2(imageName))
	indexDigest := optimizeImage(sh, mirror(imageName))

	// Pull images
	// NOTE: Registry connection will still be checked on each "run" because
	//       we added "check_always = true" to the configuration in the above.
	//       We use this behaviour for testing mirroring & refleshing functionality.
	rebootContainerd(t, sh, "", "")
	sh.X("ctr", "i", "pull", "--user", registryCreds(), mirror(imageName).ref)
	sh.X("soci", "create", mirror(imageName).ref)
	sh.X("soci", "image", "rpull", "--user", registryCreds(), "--soci-index-digest", indexDigest, mirror(imageName).ref)
	registryHostIP, registryAltHostIP := getIP(t, sh, registryHost), getIP(t, sh, registryAltHost)
	export := func(image string) []string {
		return shell.C("soci", "run", "--rm", "--snapshotter=soci", image, "test", "tar", "-c", "/usr")
	}
	sample := func(tarExportArgs ...string) {
		sh.Pipe(nil, shell.C("ctr", "run", "--rm", mirror(imageName).ref, "test", "tar", "-c", "/usr"), tarExportArgs)
	}

	// test if mirroring is working (switching to registryAltHost)
	testSameTarContents(t, sh, sample,
		func(tarExportArgs ...string) {
			sh.
				X("iptables", "-A", "OUTPUT", "-d", registryHostIP, "-j", "DROP").
				X("iptables", "-L").
				Pipe(nil, export(mirror(imageName).ref), tarExportArgs).
				X("iptables", "-D", "OUTPUT", "-d", registryHostIP, "-j", "DROP")
		},
	)

	// test if refreshing is working (swithching back to registryHost)
	testSameTarContents(t, sh, sample,
		func(tarExportArgs ...string) {
			sh.
				X("iptables", "-A", "OUTPUT", "-d", registryAltHostIP, "-j", "DROP").
				X("iptables", "-L").
				Pipe(nil, export(mirror(imageName).ref), tarExportArgs).
				X("iptables", "-D", "OUTPUT", "-d", registryAltHostIP, "-j", "DROP")
		},
	)
}

func getIP(t *testing.T, sh *shell.Shell, name string) string {
	resolved := strings.Fields(string(sh.O("getent", "hosts", name)))
	if len(resolved) < 1 {
		t.Fatalf("failed to resolve name %v", name)
	}
	return resolved[0]
}

type tarPipeExporter func(tarExportArgs ...string)

func testSameTarContents(t *testing.T, sh *shell.Shell, aC, bC tarPipeExporter) {
	aDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir A: %v", err)
	}
	bDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir B: %v", err)
	}
	aC("tar", "-xC", aDir)
	bC("tar", "-xC", bDir)
	sh.X("diff", "--no-dereference", "-qr", aDir+"/", bDir+"/")
}

type imageInfo struct {
	ref       string
	creds     string
	plainHTTP bool
}

func encodeImageInfo(ii ...imageInfo) [][]string {
	var opts [][]string
	for _, i := range ii {
		var o []string
		if i.creds != "" {
			o = append(o, "-u", i.creds)
		}
		if i.plainHTTP {
			o = append(o, "--plain-http")
		}
		o = append(o, i.ref)
		opts = append(opts, o)
	}
	return opts
}

func copyImage(sh *shell.Shell, src, dst imageInfo) {
	opts := encodeImageInfo(src, dst)
	sh.
		X(append([]string{"ctr", "i", "pull", "--all-platforms"}, opts[0]...)...).
		X("ctr", "i", "tag", src.ref, dst.ref).
		X(append([]string{"ctr", "i", "push"}, opts[1]...)...)
}

func optimizeImage(sh *shell.Shell, src imageInfo) string {
	return buildSparseIndex(sh, src, 0) // we build an index with min-layer-size 0
}

func buildSparseIndex(sh *shell.Shell, src imageInfo, minLayerSize int64) string {
	opts := encodeImageInfo(src)
	indexDigest := sh.
		X(append([]string{"ctr", "i", "pull"}, opts[0]...)...).
		X("soci", "create", src.ref, "--min-layer-size", fmt.Sprintf("%d", minLayerSize), "--oras").
		O("soci", "image", "list-indices", src.ref) // this will make SOCI artifact available locally
	return string(indexDigest)
}

func rebootContainerd(t *testing.T, sh *shell.Shell, customContainerdConfig, customSnapshotterConfig string) *testutil.RemoteSnapshotMonitor {
	var (
		containerdRoot    = "/var/lib/containerd/"
		containerdStatus  = "/run/containerd/"
		snapshotterSocket = "/run/soci-snapshotter-grpc/soci-snapshotter-grpc.sock"
		snapshotterRoot   = "/var/lib/soci-snapshotter-grpc/"
	)

	// cleanup directories
	testutil.KillMatchingProcess(sh, "containerd")
	testutil.KillMatchingProcess(sh, "soci-snapshotter-grpc")
	removeDirContents(sh, containerdRoot)
	if isDirExists(sh, containerdStatus) {
		removeDirContents(sh, containerdStatus)
	}
	if isFileExists(sh, snapshotterSocket) {
		sh.X("rm", snapshotterSocket)
	}
	if snDir := filepath.Join(snapshotterRoot, "/snapshotter/snapshots"); isDirExists(sh, snDir) {
		sh.X("find", snDir, "-maxdepth", "1", "-mindepth", "1", "-type", "d",
			"-exec", "umount", "{}/fs", ";")
	}
	removeDirContents(sh, snapshotterRoot)

	// run containerd and snapshotter
	var m *testutil.RemoteSnapshotMonitor
	containerdCmds := shell.C("containerd", "--log-level", "debug")
	if customContainerdConfig != "" {
		containerdCmds = addConfig(t, sh, customContainerdConfig, containerdCmds...)
	}
	sh.Gox(containerdCmds...)
	snapshotterCmds := shell.C("/usr/local/bin/soci-snapshotter-grpc", "--log-level", "debug",
		"--address", snapshotterSocket)
	if customSnapshotterConfig != "" {
		snapshotterCmds = addConfig(t, sh, customSnapshotterConfig, snapshotterCmds...)
	}
	outR, errR, err := sh.R(snapshotterCmds...)
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	m = testutil.NewRemoteSnapshotMonitor(testutil.NewTestingReporter(t), outR, errR)

	// make sure containerd and soci-snapshotter-grpc are up-and-running
	sh.Retry(100, "ctr", "snapshots", "--snapshotter", "soci",
		"prepare", "connectiontest-dummy-"+xid.New().String(), "")

	return m
}

func removeDirContents(sh *shell.Shell, dir string) {
	sh.X("find", dir+"/.", "!", "-name", ".", "-prune", "-exec", "rm", "-rf", "{}", "+")
}

func addConfig(t *testing.T, sh *shell.Shell, conf string, cmds ...string) []string {
	configPath := strings.TrimSpace(string(sh.O("mktemp")))
	if err := testutil.WriteFileContents(sh, configPath, []byte(conf), 0600); err != nil {
		t.Fatalf("failed to add config to %v: %v", configPath, err)
	}
	return append(cmds, "--config", configPath)
}
