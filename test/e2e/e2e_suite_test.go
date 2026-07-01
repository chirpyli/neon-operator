/*
Copyright 2025.

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"oltp.molnett.org/neon-operator/test/fixtures"
	"oltp.molnett.org/neon-operator/test/utils"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// - LIFECYCLE_TEST_SKIP=true: Skips the Neon Lifecycle integration test.
	// This test pulls large neon component images (~2+ GB total) and requires
	// significant network bandwidth. Skip by default in resource-constrained
	// environments.
	skipLifecycleTests = os.Getenv("LIFECYCLE_TEST_SKIP") == "true"

	// - NEON_IMAGE: Overrides the default neon image used in Cluster fixtures
	//   (neondatabase/neon:8463). The image is loaded into Kind as-is, so the
	//   Cluster CRD references the exact same name. Example:
	//     NEON_IMAGE=ghcr.io/neondatabase/neon:latest
	neonImage = os.Getenv("NEON_IMAGE")

	// - COMPUTE_IMAGE: Overrides the compute-node image. Because the controller
	//   hardcodes neondatabase/compute-node-v{PGVersion} (see specs/compute/),
	//   the specified image will be retagged to that name before loading into
	//   Kind. Example:
	//     COMPUTE_IMAGE=ghcr.io/neondatabase/compute-node-v17:latest
	computeImage = os.Getenv("COMPUTE_IMAGE")

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/neon-operator:v0.0.1"
)

// baseImages are small images required by the core e2e tests (manager + metrics).
var baseImages = []string{
	"curlimages/curl:latest",
}

// lifecycleImages are the heavier images required only by the Neon Lifecycle test.
// Note: neonImage and computeImage are resolved at runtime via the NEON_IMAGE env var.
var lifecycleImages = []string{
	"postgres:16-alpine",
	"minio/minio:latest",
	"minio/mc:latest",
	"busybox:latest",
}

// certManagerImages are the images required by cert-manager. Pre-pulling them
// avoids timeout on multi-node Kind clusters where each node must pull images
// from the internet independently.
var certManagerImages = []string{
	"quay.io/jetstack/cert-manager-controller:v1.16.3",
	"quay.io/jetstack/cert-manager-webhook:v1.16.3",
	"quay.io/jetstack/cert-manager-cainjector:v1.16.3",
	"quay.io/jetstack/cert-manager-startupapicheck:v1.16.3",
}

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting neon-operator-go integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the operator image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG_OPERATOR=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the operator image")

	By("loading the operator image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the operator image into Kind")

	if !skipLifecycleTests {
		// Resolve the neon image. When NEON_IMAGE is set, use it in fixtures
		// and load it into Kind as-is. The Cluster CRD references this exact
		// name, so no retag is needed.
		if neonImage != "" {
			fixtures.DefaultNeonImage = neonImage
		}

		// Resolve the compute image. The controller hardcodes
		// neondatabase/compute-node-v{PGVersion} (see specs/compute/), so when
		// COMPUTE_IMAGE points to a different name (e.g. ghcr.io/...), we must
		// retag it locally and load the renamed image into Kind.
		k8sComputeImage := fmt.Sprintf("neondatabase/compute-node-v%d", fixtures.DefaultPGVersion)
		if computeImage != "" {
			By("retagging compute image for Kind and loading")
			tagCmd := exec.Command("docker", "tag", computeImage, k8sComputeImage)
			_, err := utils.Run(tagCmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to tag %s as %s", computeImage, k8sComputeImage)

			err = utils.LoadImageToKindClusterWithName(k8sComputeImage)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(),
				"Failed to load image %s into Kind", k8sComputeImage)
		}

		// Build the full list of images to pull/load.
		imagesToLoad := append([]string{}, baseImages...)
		imagesToLoad = append(imagesToLoad, lifecycleImages...)

		// Neon image: load the fixture value (either default or NEON_IMAGE override).
		imagesToLoad = append(imagesToLoad, fixtures.DefaultNeonImage)

		// Compute image: if not overridden via COMPUTE_IMAGE, pull/load the
		// default. If overridden, it was already dealt with above.
		if computeImage == "" {
			imagesToLoad = append(imagesToLoad, k8sComputeImage)
		}

		By("pre-pulling and loading lifecycle images into Kind")
		for _, img := range imagesToLoad {
			pullCmd := exec.Command("docker", "pull", img)
			_, err := utils.Run(pullCmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull image %s", img)

			err = utils.LoadImageToKindClusterWithName(img)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load image %s into Kind", img)
		}
	} else {
		By("pre-pulling and loading base images into Kind (lifecycle test skipped)")
		for _, img := range baseImages {
			pullCmd := exec.Command("docker", "pull", img)
			_, err := utils.Run(pullCmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull image %s", img)

			err = utils.LoadImageToKindClusterWithName(img)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load image %s into Kind", img)
		}
	}

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			// Pre-pull and load cert-manager images into Kind so every node
			// can start pods immediately without pulling from the internet.
			By("pre-pulling cert-manager images into Kind")
			for _, img := range certManagerImages {
				pullCmd := exec.Command("docker", "pull", img)
				_, err := utils.Run(pullCmd)
				ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull image %s", img)

				err = utils.LoadImageToKindClusterWithName(img)
				ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load image %s into Kind", img)
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	// Teardown CertManager after the suite if not skipped and if it was not already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})
