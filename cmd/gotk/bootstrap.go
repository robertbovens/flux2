/*
Copyright 2020 The Flux CD contributors.

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

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"

	"github.com/fluxcd/toolkit/pkg/install"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bootstrap toolkit components",
	Long:  "The bootstrap sub-commands bootstrap the toolkit components on the targeted Git provider.",
}

var (
	bootstrapVersion            string
	bootstrapComponents         []string
	bootstrapRegistry           string
	bootstrapImagePullSecret    string
	bootstrapArch               string
	bootstrapBranch             string
	bootstrapWatchAllNamespaces bool
	bootstrapNetworkPolicy      bool
	bootstrapLogLevel           string
	bootstrapManifestsPath      string
	bootstrapRequiredComponents = []string{"source-controller", "kustomize-controller"}
)

const (
	bootstrapDefaultBranch         = "main"
	bootstrapInstallManifest       = "toolkit-components.yaml"
	bootstrapSourceManifest        = "toolkit-source.yaml"
	bootstrapKustomizationManifest = "toolkit-kustomization.yaml"
)

func init() {
	bootstrapCmd.PersistentFlags().StringVarP(&bootstrapVersion, "version", "v", defaultVersion,
		"toolkit version")
	bootstrapCmd.PersistentFlags().StringSliceVar(&bootstrapComponents, "components", defaultComponents,
		"list of components, accepts comma-separated values")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapRegistry, "registry", "ghcr.io/fluxcd",
		"container registry where the toolkit images are published")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapImagePullSecret, "image-pull-secret", "",
		"Kubernetes secret name used for pulling the toolkit images from a private registry")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArch, "arch", "amd64",
		"arch can be amd64 or arm64")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapBranch, "branch", bootstrapDefaultBranch,
		"default branch (for GitHub this must match the default branch setting for the organization)")
	rootCmd.AddCommand(bootstrapCmd)
	bootstrapCmd.PersistentFlags().BoolVar(&bootstrapWatchAllNamespaces, "watch-all-namespaces", true,
		"watch for custom resources in all namespaces, if set to false it will only watch the namespace where the toolkit is installed")
	bootstrapCmd.PersistentFlags().BoolVar(&bootstrapNetworkPolicy, "network-policy", true,
		"deny ingress access to the toolkit controllers from other namespaces using network policies")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapLogLevel, "log-level", "info", "set the controllers log level")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapManifestsPath, "manifests", "", "path to the manifest directory")
	bootstrapCmd.PersistentFlags().MarkHidden("manifests")
}

func bootstrapValidate() error {
	if !utils.containsItemString(supportedArch, bootstrapArch) {
		return fmt.Errorf("arch %s is not supported, can be %v", bootstrapArch, supportedArch)
	}

	if !utils.containsItemString(supportedLogLevels, bootstrapLogLevel) {
		return fmt.Errorf("log level %s is not supported, can be %v", bootstrapLogLevel, supportedLogLevels)
	}

	for _, component := range bootstrapRequiredComponents {
		if !utils.containsItemString(bootstrapComponents, component) {
			return fmt.Errorf("component %s is required", component)
		}
	}

	return nil
}

func generateInstallManifests(targetPath, namespace, tmpDir string, localManifests string) (string, error) {
	manifestsDir := path.Join(tmpDir, targetPath, namespace)
	if err := os.MkdirAll(manifestsDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("creating manifests dir failed: %w", err)
	}

	manifest := path.Join(manifestsDir, bootstrapInstallManifest)

	opts := install.Options{
		BaseURL:                localManifests,
		Version:                bootstrapVersion,
		Namespace:              namespace,
		Components:             bootstrapComponents,
		Registry:               bootstrapRegistry,
		ImagePullSecret:        bootstrapImagePullSecret,
		Arch:                   bootstrapArch,
		WatchAllNamespaces:     bootstrapWatchAllNamespaces,
		NetworkPolicy:          bootstrapNetworkPolicy,
		LogLevel:               bootstrapLogLevel,
		NotificationController: defaultNotification,
		ManifestsFile:          fmt.Sprintf("%s.yaml", namespace),
		Timeout:                timeout,
	}

	if localManifests == "" {
		opts.BaseURL = install.MakeDefaultOptions().BaseURL
	}

	output, err := install.Generate(opts)
	if err != nil {
		return "", fmt.Errorf("generating install manifests failed: %w", err)
	}

	if err := ioutil.WriteFile(manifest, output, os.ModePerm); err != nil {
		return "", fmt.Errorf("generating install manifests failed: %w", err)
	}

	return manifest, nil
}

func applyInstallManifests(ctx context.Context, manifestPath string, components []string) error {
	kubectlArgs := []string{"apply", "-f", manifestPath}
	if _, err := utils.execKubectlCommand(ctx, ModeOS, kubectlArgs...); err != nil {
		return fmt.Errorf("install failed")
	}

	for _, deployment := range components {
		kubectlArgs = []string{"-n", namespace, "rollout", "status", "deployment", deployment, "--timeout", timeout.String()}
		if _, err := utils.execKubectlCommand(ctx, ModeOS, kubectlArgs...); err != nil {
			return fmt.Errorf("install failed")
		}
	}
	return nil
}

func generateSyncManifests(url, branch, name, namespace, targetPath, tmpDir string, interval time.Duration) error {
	gvk := sourcev1.GroupVersion.WithKind(sourcev1.GitRepositoryKind)
	gitRepository := sourcev1.GitRepository{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.Kind,
			APIVersion: gvk.GroupVersion().String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: url,
			Interval: metav1.Duration{
				Duration: interval,
			},
			Reference: &sourcev1.GitRepositoryRef{
				Branch: branch,
			},
			SecretRef: &corev1.LocalObjectReference{
				Name: name,
			},
		},
	}

	gitData, err := yaml.Marshal(gitRepository)
	if err != nil {
		return err
	}

	if err := utils.writeFile(string(gitData), filepath.Join(tmpDir, targetPath, namespace, bootstrapSourceManifest)); err != nil {
		return err
	}

	gvk = kustomizev1.GroupVersion.WithKind(kustomizev1.KustomizationKind)
	kustomization := kustomizev1.Kustomization{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.Kind,
			APIVersion: gvk.GroupVersion().String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{
				Duration: 10 * time.Minute,
			},
			Path:  fmt.Sprintf("./%s", strings.TrimPrefix(targetPath, "./")),
			Prune: true,
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Kind: sourcev1.GitRepositoryKind,
				Name: name,
			},
			Validation: "client",
		},
	}

	ksData, err := yaml.Marshal(kustomization)
	if err != nil {
		return err
	}

	if err := utils.writeFile(string(ksData), filepath.Join(tmpDir, targetPath, namespace, bootstrapKustomizationManifest)); err != nil {
		return err
	}

	if err := utils.generateKustomizationYaml(filepath.Join(tmpDir, targetPath, namespace)); err != nil {
		return err
	}

	return nil
}

func applySyncManifests(ctx context.Context, kubeClient client.Client, name, namespace, targetPath, tmpDir string) error {
	kubectlArgs := []string{"apply", "-k", filepath.Join(tmpDir, targetPath, namespace)}
	if _, err := utils.execKubectlCommand(ctx, ModeStderrOS, kubectlArgs...); err != nil {
		return err
	}

	logger.Waitingf("waiting for cluster sync")

	if err := wait.PollImmediate(pollInterval, timeout,
		isGitRepositoryReady(ctx, kubeClient, name, namespace)); err != nil {
		return err
	}

	if err := wait.PollImmediate(pollInterval, timeout,
		isKustomizationReady(ctx, kubeClient, name, namespace)); err != nil {
		return err
	}

	return nil
}

func shouldInstallManifests(ctx context.Context, kubeClient client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}
	var kustomization kustomizev1.Kustomization
	if err := kubeClient.Get(ctx, namespacedName, &kustomization); err != nil {
		return true
	}

	return kustomization.Status.LastAppliedRevision == ""
}

func shouldCreateDeployKey(ctx context.Context, kubeClient client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}

	var existing corev1.Secret
	if err := kubeClient.Get(ctx, namespacedName, &existing); err != nil {
		return true
	}
	return false
}

func generateDeployKey(ctx context.Context, kubeClient client.Client, url *url.URL, namespace string) (string, error) {
	pair, err := generateKeyPair(ctx)
	if err != nil {
		return "", err
	}

	hostKey, err := scanHostKey(ctx, url)
	if err != nil {
		return "", err
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace,
			Namespace: namespace,
		},
		StringData: map[string]string{
			"identity":     string(pair.PrivateKey),
			"identity.pub": string(pair.PublicKey),
			"known_hosts":  string(hostKey),
		},
	}
	if err := upsertSecret(ctx, kubeClient, secret); err != nil {
		return "", err
	}

	return string(pair.PublicKey), nil
}
