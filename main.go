package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	// Kubernetes auth plugins (Azure, GCP, OIDC, ...).

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/pointer"

	helmaction "helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	helmcfg "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/releaseutil"

	"dario.cat/mergo"
	v2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"github.com/databus23/helm-diff/v3/diff"
	"github.com/databus23/helm-diff/v3/manifest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/cli-runtime/pkg/printers"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxchartutil "github.com/fluxcd/pkg/chartutil"
	"github.com/fluxcd/pkg/runtime/conditions"
	hchart "helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Global CLI flags.
var Version = "dev"

var (
	kubeconfig  string
	kubecontext string
	ns          string
	chDir       string
	plain       bool     // render without talking to the API server
	showOnly    []string // template globs for `cozypkg show`
	extraVals   []string // additional -f/--values files
)

func init() {
	_ = v2.AddToScheme(clientsetscheme.Scheme)
	_ = sourcev1.AddToScheme(clientsetscheme.Scheme)
	_ = metav1.AddMetaToScheme(clientsetscheme.Scheme)
}

// main is the application entry-point.
func main() {
	log.SetFlags(0)

	root := &cobra.Command{
		Use:     "cozypkg",
		Short:   "Cozy wrapper around Helm and Flux CD for local development",
		Version: Version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if chDir != "" {
				err := os.Chdir(chDir)
				if err != nil {
					log.Fatalf("could not chdir to %s: %v", chDir, err)
				}
			}
		},
	}
	root.SetVersionTemplate("cozypkg version {{.Version}}\n")

	root.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	root.PersistentFlags().StringVar(&kubecontext, "context", "", "Kube context")
	root.PersistentFlags().StringVarP(&ns, "namespace", "n", "", "Kubernetes namespace (defaults to the current context)")
	root.PersistentFlags().StringVarP(&chDir, "working-directory", "C", "", "Root directory of Helm chart to run against (defaults to current directory)")

	_ = root.RegisterFlagCompletionFunc("namespace", completeNamespaces)

	root.AddCommand(
		cmdShow(),
		cmdApply(),
		cmdDiff(),
		cmdSuspend(),
		cmdResume(),
		cmdDelete(),
		cmdList(),
		cmdGet(),
		cmdCompletion(),
		cmdReconcile(),
	)

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("cozypkg", Version)
		},
	})

	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.Execute(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

// loadClientConfig builds a client config with optional kubeconfig and context overrides.
func loadClientConfig() clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}
	if ns != "" {
		overrides.Context.Namespace = ns
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
}

// restConfig builds a *rest.Config from the --kubeconfig flag or $KUBECONFIG.
func restConfig() *rest.Config {
	config, err := loadClientConfig().ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading kubeconfig: %v\n", err)
		os.Exit(1)
	}
	return config
}

// helmCfg returns an initialised Helm configuration bound to a namespace.
func helmCfg(rc *rest.Config, namespace string) (*helmaction.Configuration, *helmcfg.EnvSettings, error) {
	env := helmcfg.New()
	if kubeconfig != "" {
		env.KubeConfig = kubeconfig
	}
	env.SetNamespace(namespace)

	cfg := new(helmaction.Configuration)
	if err := cfg.Init(env.RESTClientGetter(), namespace, "secret", log.Printf); err != nil {
		return nil, nil, err
	}
	return cfg, env, nil
}

// fluxPostRenderer injects Flux labels so that rendered manifests match the
// server-side state.
type fluxPostRenderer struct{ name, ns string }

// Run adds Flux labels to every object.
func (f *fluxPostRenderer) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	docs := bytes.Split(in.Bytes(), []byte("\n---"))
	var out [][]byte

	for _, d := range docs {
		if len(bytes.TrimSpace(d)) == 0 {
			continue
		}

		var obj map[string]interface{}
		if err := sigsyaml.Unmarshal(d, &obj); err != nil || obj == nil {
			// keep lists / empty docs intact
			out = append(out, d)
			continue
		}

		md := ensureMap(obj, "metadata")
		lbl := ensureMap(md, "labels")
		lbl["helm.toolkit.fluxcd.io/name"] = f.name
		lbl["helm.toolkit.fluxcd.io/namespace"] = f.ns

		rendered, err := sigsyaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, rendered)
	}

	return bytes.NewBuffer(bytes.Join(out, []byte("\n---\n"))), nil
}

// ensureMap returns the map under parent[key], creating it if needed.
func ensureMap(parent map[string]interface{}, key string) map[string]interface{} {
	if parent == nil {
		return map[string]interface{}{}
	}
	if v, ok := parent[key]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	m := map[string]interface{}{}
	parent[key] = m
	return m
}

// resolveChartRef resolves a chartRef to get chart information (name, version, valuesFiles).
// Note: chart directory is expected to be available locally, this function only gets metadata.
func resolveChartRef(ctx context.Context, cl client.Client, hr *v2.HelmRelease) (chartName, chartVersion string, valuesFiles []string, err error) {
	if hr.Spec.ChartRef == nil {
		return "", "", nil, fmt.Errorf("chartRef is nil")
	}

	refNS := hr.Spec.ChartRef.Namespace
	if refNS == "" {
		refNS = hr.Namespace
	}

	refName := hr.Spec.ChartRef.Name
	refKind := hr.Spec.ChartRef.Kind

	switch refKind {
	case "ExternalArtifact":
		// ExternalArtifact - fetch the resource to extract chart name from artifact path
		extArtifactGVR := schema.GroupVersionResource{
			Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "externalartifacts",
		}
		extArtifact := &unstructured.Unstructured{}
		extArtifact.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   extArtifactGVR.Group,
			Version: extArtifactGVR.Version,
			Kind:    "ExternalArtifact",
		})
		if err := cl.Get(ctx, types.NamespacedName{Namespace: refNS, Name: refName}, extArtifact); err != nil {
			return "", "", nil, fmt.Errorf("failed to get ExternalArtifact %s/%s: %w", refNS, refName, err)
		}

		// Extract chart name from artifact path: externalartifact/{namespace}/{name}/{revision}.tar.gz
		// The chart name is the third segment (index 2) in the path
		artifactPath, found, _ := unstructured.NestedString(extArtifact.Object, "status", "artifact", "path")
		if !found || artifactPath == "" {
			return "", "", nil, fmt.Errorf("ExternalArtifact %s/%s does not have artifact path in status", refNS, refName)
		}
		parts := strings.Split(artifactPath, "/")
		// Path format: externalartifact/{namespace}/{name}/{revision}.tar.gz
		// We want the {name} part (index 2)
		if len(parts) < 3 {
			return "", "", nil, fmt.Errorf("ExternalArtifact %s/%s has unexpected artifact path format: %s", refNS, refName, artifactPath)
		}
		chartName = parts[2]
		return chartName, "", nil, nil

	case "HelmChart":
		chart := &sourcev1.HelmChart{}
		if err := cl.Get(ctx, types.NamespacedName{Namespace: refNS, Name: refName}, chart); err != nil {
			return "", "", nil, fmt.Errorf("failed to get HelmChart %s/%s: %w", refNS, refName, err)
		}

		chartName = chart.Spec.Chart
		chartVersion = chart.Spec.Version
		valuesFiles = chart.Spec.ValuesFiles
		return chartName, chartVersion, valuesFiles, nil

	case "OCIRepository":
		oci := &sourcev1.OCIRepository{}
		if err := cl.Get(ctx, types.NamespacedName{Namespace: refNS, Name: refName}, oci); err != nil {
			return "", "", nil, fmt.Errorf("failed to get OCIRepository %s/%s: %w", refNS, refName, err)
		}

		// Extract chart name from OCI URL or use reference name
		chartName = refName
		if oci.Spec.URL != "" {
			parts := strings.Split(strings.TrimPrefix(oci.Spec.URL, "oci://"), "/")
			if len(parts) > 0 {
				chartName = parts[len(parts)-1]
			}
		}

		return chartName, "", nil, nil

	default:
		return "", "", nil, fmt.Errorf("unsupported chartRef kind: %s", refKind)
	}
}

// findArtifactGeneratorForExternalArtifact finds the ArtifactGenerator that created the given ExternalArtifact
// by searching through ArtifactGenerators' inventory.
func findArtifactGeneratorForExternalArtifact(ctx context.Context, cl client.Client, extArtifactNS, extArtifactName string) (*unstructured.Unstructured, error) {
	dyn, err := dynamic.NewForConfig(restConfig())
	if err != nil {
		return nil, err
	}

	artifactGenGVR := schema.GroupVersionResource{
		Group: "source.extensions.fluxcd.io", Version: "v1beta1", Resource: "artifactgenerators",
	}

	list, err := dyn.Resource(artifactGenGVR).Namespace(extArtifactNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("failed to list ArtifactGenerators with GVR %s: %w", artifactGenGVR, err)
		}
		artifactGenGVR = schema.GroupVersionResource{
			Group: "source.watcher.fluxcd.io", Version: "v2", Resource: "artifactgenerators",
		}
		list, err = dyn.Resource(artifactGenGVR).Namespace(extArtifactNS).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list ArtifactGenerators with GVR %s: %w", artifactGenGVR, err)
		}
	}

	for _, item := range list.Items {
		inventory, found, _ := unstructured.NestedSlice(item.Object, "status", "inventory")
		if found {
			for _, inv := range inventory {
				invMap, ok := inv.(map[string]interface{})
				if !ok {
					continue
				}
				name, _, _ := unstructured.NestedString(invMap, "name")
				if name == extArtifactName {
					return &item, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("ArtifactGenerator for ExternalArtifact %s/%s not found", extArtifactNS, extArtifactName)
}

// getChartInfo gets chart name and version from chart or chartRef.
func getChartInfo(ctx context.Context, cl client.Client, hr *v2.HelmRelease) (chartName, chartVersion string, err error) {
	if hr.Spec.ChartRef != nil {
		name, version, _, err := resolveChartRef(ctx, cl, hr)
		if err != nil {
			return "", "", err
		}
		return name, version, nil
	}

	if hr.Spec.Chart != nil {
		return hr.Spec.Chart.Spec.Chart, hr.Spec.Chart.Spec.Version, nil
	}

	return "", "", fmt.Errorf("neither chart nor chartRef is set")
}

// mergedValues merges valuesFiles and inline .spec.values in the given HelmRelease.
func mergedValues(ctx context.Context, cl client.Client, hr *v2.HelmRelease, chartDir string) (map[string]interface{}, error) {
	vals := map[string]interface{}{}

	var valuesFiles []string

	// Get valuesFiles from chart or chartRef
	if hr.Spec.ChartRef != nil {
		_, _, vf, err := resolveChartRef(ctx, cl, hr)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve chartRef: %w", err)
		}
		valuesFiles = vf
	} else if hr.Spec.Chart != nil {
		valuesFiles = hr.Spec.Chart.Spec.ValuesFiles
	}

	// Merge valuesFiles
	for _, vf := range valuesFiles {
		if vf == "-" { // stdin placeholder – not applicable here
			continue
		}
		data, err := os.ReadFile(filepath.Join(chartDir, vf))
		if err != nil {
			return nil, err
		}
		var mv map[string]interface{}
		if err := sigsyaml.Unmarshal(data, &mv); err != nil {
			return nil, err
		}
		if err := mergo.Merge(&vals, mv, mergo.WithOverride); err != nil {
			return nil, err
		}
	}

	// Merge inline values
	if hr.Spec.Values != nil && len(hr.Spec.Values.Raw) > 0 {
		var mv map[string]interface{}
		if err := sigsyaml.Unmarshal(hr.Spec.Values.Raw, &mv); err != nil {
			return nil, err
		}
		if err := mergo.Merge(&vals, mv, mergo.WithOverride); err != nil {
			return nil, err
		}
	}

	return vals, nil
}

// renderManifests performs a Helm dry-run render and returns the manifest text.
func renderManifests(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string, vals map[string]interface{}, rc *rest.Config) (string, error) {
	inst := helmaction.NewInstall(cfg)
	inst.DryRun = true
	inst.DryRunOption = "server"
	inst.ReleaseName = hr.Name
	inst.Namespace = hr.Namespace
	inst.DisableHooks = true

	if plain {
		kubeVer, err := discoverKubeVersion(rc)
		if err != nil {
			return "", err
		}
		inst.KubeVersion = &kubeVer
		vers, err := discoverAPIVersions(rc)
		if err != nil {
			return "", err
		}
		inst.APIVersions = vers
		inst.ClientOnly = true
	} else {
		inst.PostRenderer = &fluxPostRenderer{name: hr.Name, ns: hr.Namespace}
	}

	ch, err := loader.Load(chartDir)
	if err != nil {
		return "", err
	}
	rel, err := inst.Run(ch, vals)
	if err != nil {
		return "", err
	}
	return rel.Manifest, nil
}

// upgradeRelease runs a Helm upgrade (installing if necessary).
func upgradeRelease(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string, vals map[string]interface{}, takeOwnership bool) error {
	relName := hr.Name
	namespace := hr.Namespace

	hist := helmaction.NewHistory(cfg)
	hist.Max = 1
	_, err := hist.Run(relName)

	isNotFound := err != nil && strings.Contains(err.Error(), "release: not found")

	ch, err := loader.Load(chartDir)
	if err != nil {
		return err
	}

	if isNotFound {
		inst := helmaction.NewInstall(cfg)
		inst.Namespace = namespace
		inst.ReleaseName = relName
		inst.TakeOwnership = takeOwnership
		if !plain {
			inst.PostRenderer = &fluxPostRenderer{name: relName, ns: namespace}
		}
		_, err = inst.Run(ch, vals)
		return err
	}

	up := helmaction.NewUpgrade(cfg)
	up.Namespace = namespace
	up.Install = true // informative only
	up.TakeOwnership = takeOwnership
	if !plain {
		up.PostRenderer = &fluxPostRenderer{name: relName, ns: namespace}
	}

	_, err = up.Run(relName, ch, vals)
	return err
}

// realHelmDiff returns a textual diff between the live release and desired state.
func realHelmDiff(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string, vals map[string]interface{}) (string, error) {
	var buf bytes.Buffer

	get := helmaction.NewGet(cfg)
	rel, err := get.Run(hr.Name)
	var current []byte
	if err == nil {
		current = []byte(rel.Manifest)
	} else if !strings.Contains(err.Error(), "release: not found") {
		return "", fmt.Errorf("failed to get release: %w", err)
	}

	inst := helmaction.NewInstall(cfg)
	inst.DryRun = true
	inst.DryRunOption = "server"
	inst.ClientOnly = true
	inst.ReleaseName = hr.Name
	inst.Namespace = hr.Namespace
	inst.DisableHooks = true

	rc := restConfig()
	kubeVer, err := discoverKubeVersion(rc)
	if err != nil {
		return "", err
	}
	inst.KubeVersion = &kubeVer
	vers, err := discoverAPIVersions(rc)
	if err != nil {
		return "", err
	}
	inst.APIVersions = vers
	inst.ClientOnly = true
	inst.PostRenderer = &fluxPostRenderer{name: hr.Name, ns: hr.Namespace}

	ch, err := loader.Load(chartDir)
	if err != nil {
		return "", err
	}
	dry, err := inst.Run(ch, vals)
	if err != nil {
		return "", err
	}
	desired := []byte(dry.Manifest)

	curSpecs := manifest.Parse(string(current), hr.Namespace, false, manifest.Helm3TestHook, manifest.Helm2TestSuccessHook)
	newSpecs := manifest.Parse(string(desired), hr.Namespace, false, manifest.Helm3TestHook, manifest.Helm2TestSuccessHook)

	_ = diff.Manifests(curSpecs, newSpecs, &diff.Options{OutputContext: -1, ShowSecrets: true}, &buf)
	return buf.String(), nil
}

// runFn is a helper signature passed to cmdFactory.
type runFn func(context.Context, client.Client, *helmaction.Configuration, *v2.HelmRelease, string) error

// cmdFactory fetches (or stubs) a HelmRelease and invokes runFn.
func cmdFactory(name string, fn runFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:  name + " <release>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			relName := args[0]

			if name == "show" || name == "diff" || name == "apply" {
				chartDir, err := filepath.Abs(".")
				if err != nil {
					return fmt.Errorf("unable to determine current directory: %w", err)
				}
				if ok, err := hchart.IsChartDir(chartDir); err != nil || !ok {
					return fmt.Errorf("invalid Helm chart in %q: %w", chartDir, err)
				}
			}

			var err error
			rc := restConfig()

			ctx := context.TODO()

			// Offline mode: create a minimal stub.
			if plain {
				if ns == "" {
					ns = "default"
				}
				stub := &v2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: relName, Namespace: ns}}
				cfg, _, err := helmCfg(rc, ns)
				if err != nil {
					return err
				}
				// Create a dummy client for plain mode
				cl, err := client.New(rc, client.Options{})
				if err != nil {
					return err
				}
				return fn(ctx, cl, cfg, stub, ".")
			}

			if ns == "" {
				ns, _, err = defaultNamespace()
				if err != nil {
					return err
				}
			}

			cfg, _, err := helmCfg(rc, ns)
			if err != nil {
				return err
			}

			cl, err := client.New(rc, client.Options{})
			if err != nil {
				return err
			}

			var hr v2.HelmRelease
			if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: relName}, &hr); err != nil {
				return err
			}

			return fn(ctx, cl, cfg, &hr, ".")
		},
	}
	cmd.ValidArgsFunction = completeHelmReleases
	return cmd
}

// cmdShow returns the `cozypkg show` command.
func cmdShow() *cobra.Command {
	cmd := cmdFactory("show", func(ctx context.Context, cl client.Client, cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		var vals map[string]interface{}
		if plain {
			vals = map[string]interface{}{}
		} else {
			var err error
			vals, err = mergedValues(ctx, cl, hr, chartDir)
			if err != nil {
				return err
			}
		}

		if add, err := loadExtraValues(); err != nil {
			return err
		} else if err := mergo.Merge(&vals, add, mergo.WithOverride); err != nil {
			return err
		}

		var err error
		rc := restConfig()

		mani, err := renderManifests(cfg, hr, chartDir, vals, rc)
		if err != nil {
			return err
		}

		if len(showOnly) == 0 {
			fmt.Print(mani)
			return nil
		}

		split := releaseutil.SplitManifests(mani)
		keys := make([]string, 0, len(split))
		for k := range split {
			keys = append(keys, k)
		}
		sort.Sort(releaseutil.BySplitManifestsOrder(keys))

		reSrc := regexp.MustCompile(`# Source: [^/]+/(.+)`) // emulate helm --show-only
		for _, glob := range showOnly {
			found := false
			for _, k := range keys {
				m := split[k]
				sm := reSrc.FindStringSubmatch(m)
				if len(sm) == 0 {
					continue
				}
				if ok, _ := filepath.Match(filepath.ToSlash(glob), sm[1]); !ok {
					continue
				}
				fmt.Printf("---\n%s\n", m)
				found = true
			}
			if !found {
				return fmt.Errorf("template %s not found in chart", glob)
			}
		}
		return nil
	})
	cmd.Short = "Render manifests like helm template"
	cmd.Flags().StringSliceVarP(&showOnly, "show-only", "s", nil, "Render only templates matching glob(s)")
	cmd.Flags().StringSliceVarP(&extraVals, "values", "f", nil, "Additional values files (may be repeated)")
	cmd.Flags().BoolVar(&plain, "plain", false, "Render chart without querying values from the HelmRelease")
	return cmd
}

// cmdApply returns the `cozypkg apply` command.
func cmdApply() *cobra.Command {
	var autoResume bool
	var takeOwnership bool
	cmd := cmdFactory("apply", func(ctx context.Context, cl client.Client, cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		if plain && autoResume {
			return fmt.Errorf("--resume may not be used with --plain")
		}

		bc := record.NewBroadcaster()
		defer bc.Shutdown()
		rec := bc.NewRecorder(clientsetscheme.Scheme, corev1.EventSource{Component: "cozypkg"})
		vals := map[string]interface{}{}

		if !plain {
			// Suspend before touching Helm.
			if err := patchSuspend(ctx, cl, hr.Namespace, hr.Name, pointer.Bool(true)); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "HelmRelease %s/%s suspended\n", hr.Namespace, hr.Name)
			// Merge values from the HelmRelease.
			var err error
			vals, err = mergedValues(ctx, cl, hr, chartDir)
			if err != nil {
				return err
			}
		}

		if add, err := loadExtraValues(); err != nil {
			return err
		} else if err := mergo.Merge(&vals, add, mergo.WithOverride); err != nil {
			return err
		}

		var chartVer, cfgDigest string
		if !plain {
			cfgDigest = fluxchartutil.DigestValues(digest.Canonical, vals).String()
			_, ver, err := getChartInfo(ctx, cl, hr)
			if err == nil {
				chartVer = ver
			}

			hr.Status.LastAttemptedGeneration = hr.Generation
			hr.Status.LastAttemptedRevision = chartVer
			hr.Status.LastAttemptedConfigDigest = cfgDigest
			hr.Status.LastAttemptedReleaseAction = v2.ReleaseActionUpgrade
			_ = cl.Status().Update(ctx, hr)
		}

		if err := upgradeRelease(cfg, hr, chartDir, vals, takeOwnership); err != nil {
			markFailure(ctx, cl, nil, hr, err)
			return err
		}

		if autoResume {
			if err := patchSuspend(ctx, cl, hr.Namespace, hr.Name, nil); err != nil {
				return err
			}
		}

		if !plain {
			markSuccess(ctx, cl, rec, hr, chartVer, cfgDigest)
		}
		return nil
	})

	cmd.Short = "Upgrade or install HelmRelease and sync status"
	cmd.Flags().BoolVar(&plain, "plain", false, "Install chart without querying values from the HelmRelease")
	cmd.Flags().BoolVar(&autoResume, "resume", false, "Automatically clear spec.suspend after successful apply")
	cmd.Flags().StringSliceVarP(&extraVals, "values", "f", nil, "Additional values files (may be repeated)")
	cmd.Flags().BoolVar(&takeOwnership, "take-ownership", false, "Take ownership of existing resources")
	return cmd
}

// cmdDiff returns the `cozypkg diff` command.
func cmdDiff() *cobra.Command {
	cmd := cmdFactory("diff", func(ctx context.Context, cl client.Client, cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		var vals map[string]interface{}
		if plain {
			vals = map[string]interface{}{}
		} else {
			var err error
			vals, err = mergedValues(ctx, cl, hr, chartDir)
			if err != nil {
				return err
			}
		}

		if add, err := loadExtraValues(); err != nil {
			return err
		} else if err := mergo.Merge(&vals, add, mergo.WithOverride); err != nil {
			return err
		}

		out, err := realHelmDiff(cfg, hr, chartDir, vals)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	})

	cmd.Short = "Show a diff between live and desired manifests"
	cmd.Flags().StringSliceVarP(&extraVals, "values", "f", nil, "Additional values files (may be repeated)")
	cmd.Flags().BoolVar(&plain, "plain", false, "Render chart without querying values from the HelmRelease")
	return cmd
}

// cmdSuspend returns the `cozypkg suspend` command.
func cmdSuspend() *cobra.Command {
	cmd := cmdFactory("suspend", func(ctx context.Context, cl client.Client, _ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		return patchSuspend(ctx, cl, hr.Namespace, hr.Name, pointer.Bool(true))
	})
	cmd.Short = "Suspend Flux HelmRelease"
	return cmd
}

// cmdResume returns the `cozypkg resume` command.
func cmdResume() *cobra.Command {
	cmd := cmdFactory("resume", func(ctx context.Context, cl client.Client, _ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		return patchSuspend(ctx, cl, hr.Namespace, hr.Name, nil)
	})
	cmd.Short = "Resume Flux HelmRelease"
	return cmd
}

// cmdDelete returns the `cozypkg delete` command.
func cmdDelete() *cobra.Command {
	cmd := cmdFactory("delete", func(_ context.Context, _ client.Client, cfg *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		un := helmaction.NewUninstall(cfg)
		_, err := un.Run(hr.Name)
		return err
	})
	cmd.Short = "Uninstall the Helm release"
	return cmd
}

// defaultNamespace returns the namespace from the kubeconfig or "default".
func defaultNamespace() (string, bool, error) {
	return loadClientConfig().Namespace()
}

// serverTable retrieves a metav1.Table from the API server.
func serverTable(cfg *rest.Config, gvr schema.GroupVersionResource, namespace, name string) (*metav1.Table, error) {
	rcfg := rest.CopyConfig(cfg)
	rcfg.APIPath = "/apis"
	rcfg.GroupVersion = &schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}
	rcfg.NegotiatedSerializer = clientsetscheme.Codecs.WithoutConversion()

	rc, err := rest.RESTClientFor(rcfg)
	if err != nil {
		return nil, err
	}

	req := rc.Get()
	if namespace != "" {
		req = req.Namespace(namespace)
	}
	req = req.Resource(gvr.Resource)
	if name != "" {
		req = req.Name(name)
	}

	req.SetHeader("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1,application/json")
	req.Param("includeObject", "Object")

	raw, err := req.DoRaw(context.TODO())
	if err != nil {
		return nil, err
	}

	obj, _, err := clientsetscheme.Codecs.UniversalDeserializer().Decode(raw, nil, nil)
	if err != nil {
		return nil, err
	}

	table, ok := obj.(*metav1.Table)
	if !ok {
		return nil, fmt.Errorf("unexpected object kind: %T", obj)
	}
	return table, nil
}

// prependNamespaceColumn adds the NAMESPACE column when listing across all namespaces.
func prependNamespaceColumn(t *metav1.Table) error {
	if len(t.ColumnDefinitions) > 0 && strings.EqualFold(t.ColumnDefinitions[0].Name, "NAMESPACE") {
		return nil
	}

	nsCol := metav1.TableColumnDefinition{Name: "NAMESPACE", Type: "string"}
	t.ColumnDefinitions = append([]metav1.TableColumnDefinition{nsCol}, t.ColumnDefinitions...)

	for i, row := range t.Rows {
		ns := ""
		if row.Object.Object != nil {
			if acc, err := meta.Accessor(row.Object.Object); err == nil {
				ns = acc.GetNamespace()
			}
		} else if len(row.Object.Raw) > 0 {
			if decoded, _, err := clientsetscheme.Codecs.UniversalDeserializer().Decode(row.Object.Raw, nil, nil); err == nil {
				if acc, err := meta.Accessor(decoded); err == nil {
					ns = acc.GetNamespace()
				}
			}
		}
		row.Cells = append([]interface{}{ns}, row.Cells...)
		t.Rows[i] = row
	}
	return nil
}

// runHRCommand implements shared logic for `get` and `list`.
func runHRCommand(cmd *cobra.Command, args []string, allNS bool, output *string) error {
	if allNS && len(args) > 0 {
		return fmt.Errorf("-A/--all-namespaces may be used only when no names are specified")
	}

	var err error
	rc := restConfig()

	nsLocal := ns
	if nsLocal == "" && !allNS {
		nsLocal, _, err = defaultNamespace()
		if err != nil {
			return err
		}
	}

	gvr := schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	wantTable := *output == "" || *output == "wide"

	obj, err := collectHR(cmd.Context(), rc, gvr, nsLocal, allNS, args, wantTable)
	if err != nil {
		return err
	}

	if wantTable {
		tp := printers.NewTablePrinter(printers.PrintOptions{Wide: *output == "wide"})
		return tp.PrintObj(obj, cmd.OutOrStdout())
	}

	pf := genericclioptions.NewPrintFlags("").WithTypeSetter(clientsetscheme.Scheme)
	pf.OutputFormat = output
	pr, _ := pf.ToPrinter()
	return pr.PrintObj(obj, cmd.OutOrStdout())
}

// collectHR aggregates HelmRelease objects (raw or as tables) depending on the
// requested output.
func collectHR(
	ctx context.Context,
	rc *rest.Config,
	gvr schema.GroupVersionResource,
	ns string,
	allNS bool,
	names []string, // nil | len==0 → list current NS / cluster
	wantTable bool,
) (runtime.Object, error) {

	// ─── helpers ------------------------------------------------------------
	dc, _ := dynamic.NewForConfig(rc)

	fetchTable := func(targetNS, name string) (*metav1.Table, error) {
		return serverTable(rc, gvr, targetNS, name)
	}
	fetchObj := func(targetNS, name string) (*unstructured.Unstructured, error) {
		if name == "" { // list
			list, err := dc.Resource(gvr).Namespace(targetNS).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			ul := &unstructured.Unstructured{}
			ul.Object = list.Object
			return ul, nil
		}
		return dc.Resource(gvr).Namespace(targetNS).Get(ctx, name, metav1.GetOptions{})
	}

	// ─── table branch -------------------------------------------------------
	if wantTable {
		var agg *metav1.Table
		operate := func(targetNS, name string) error {
			tbl, err := fetchTable(targetNS, name)
			if err != nil {
				return err
			}
			if allNS {
				_ = prependNamespaceColumn(tbl)
			}
			if agg == nil {
				agg = tbl
			} else {
				agg.Rows = append(agg.Rows, tbl.Rows...)
			}
			return nil
		}

		if len(names) == 0 { // list
			targetNS := ""
			if !allNS {
				targetNS = ns
			}
			if err := operate(targetNS, ""); err != nil {
				return nil, err
			}
			return agg, nil
		}

		for _, n := range names {
			targetNS := ns
			if allNS {
				targetNS = ""
			}
			if err := operate(targetNS, n); err != nil {
				return nil, err
			}
		}
		return agg, nil
	}

	// ─── raw branch ---------------------------------------------------------
	ulist := &unstructured.UnstructuredList{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "List",
	}}

	if len(names) == 0 { // list
		targetNS := ""
		if !allNS {
			targetNS = ns
		}
		u, err := fetchObj(targetNS, "")
		if err != nil {
			return nil, err
		}
		for i := range u.Object["items"].([]interface{}) {
			item := u.Object["items"].([]interface{})[i].(map[string]interface{})
			ulist.Items = append(ulist.Items, unstructured.Unstructured{Object: item})
		}
		return ulist, nil
	}

	if len(names) == 1 {
		targetNS := ns
		if allNS {
			targetNS = ""
		}
		return fetchObj(targetNS, names[0])
	}

	for _, n := range names {
		targetNS := ns
		if allNS {
			targetNS = ""
		}
		u, err := fetchObj(targetNS, n)
		if err != nil {
			return nil, err
		}
		ulist.Items = append(ulist.Items, *u)
	}
	return ulist, nil
}

// cmdGet exposes `cozypkg get`.
func cmdGet() *cobra.Command {
	var (
		allNS  bool
		output string
	)

	cmd := &cobra.Command{
		Use:   "get [release...]",
		Short: "Get one or many HelmReleases",
		Args:  cobra.ArbitraryArgs,
		RunE:  func(c *cobra.Command, args []string) error { return runHRCommand(c, args, allNS, &output) },
	}
	cmd.ValidArgsFunction = completeHelmReleases

	cmd.Flags().BoolVarP(&allNS, "all-namespaces", "A", false, "Across all namespaces")
	cmd.Flags().StringVarP(&output, "output", "o", "", "json|yaml|wide|name|custom-columns=<...>")
	return cmd
}

// cmdList exposes `cozypkg list` (alias: ls).
func cmdList() *cobra.Command {
	var (
		allNS  bool
		output string
	)

	cmd := &cobra.Command{
		Use:     "list [release...]",
		Aliases: []string{"ls"},
		Short:   "List HelmReleases",
		Args:    cobra.ArbitraryArgs,
		RunE:    func(c *cobra.Command, args []string) error { return runHRCommand(c, args, allNS, &output) },
	}
	cmd.ValidArgsFunction = completeHelmReleases

	cmd.Flags().BoolVarP(&allNS, "all-namespaces", "A", false, "Across all namespaces (only when no names are given)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "json|yaml|wide|name|custom-columns=<...>")
	return cmd
}

// newHistoryEntry creates a v2.Snapshot for status.history.
func newHistoryEntry(ctx context.Context, cl client.Client, hr *v2.HelmRelease, chartVersion, cfgDigest string) *v2.Snapshot {
	chartName, _, err := getChartInfo(ctx, cl, hr)
	if err != nil {
		// Log the error and fall back to the HelmRelease name.
		log.Printf("could not get chart info for %s/%s: %v", hr.Namespace, hr.Name, err)
		chartName = hr.Name
	}
	return &v2.Snapshot{
		Name:          chartName,
		Namespace:     hr.Namespace,
		Version:       1,
		ChartName:     chartName,
		ChartVersion:  chartVersion,
		ConfigDigest:  cfgDigest,
		FirstDeployed: metav1.Now(),
		LastDeployed:  metav1.Now(),
		Status:        "deployed",
	}
}

// markSuccess sets Ready=True and emits a normal event.
func markSuccess(ctx context.Context, cl client.Client, rec record.EventRecorder, hr *v2.HelmRelease, chartVer, cfgDigest string) {
	chartName, _, err := getChartInfo(ctx, cl, hr)
	if err != nil {
		// Log the error and fall back to the HelmRelease name for the message.
		log.Printf("could not get chart info for %s/%s: %v", hr.Namespace, hr.Name, err)
		chartName = hr.Name
	}
	msg := fmt.Sprintf("Helm upgrade succeeded for %s/%s with chart %s@%s", hr.Namespace, hr.Name, chartName, chartVer)

	conditions.MarkTrue(hr, v2.ReleasedCondition, v2.UpgradeSucceededReason, msg)
	conditions.MarkTrue(hr, fluxmeta.ReadyCondition, v2.UpgradeSucceededReason, msg)

	hr.Status.History = append(hr.Status.History, newHistoryEntry(ctx, cl, hr, chartVer, cfgDigest))
	hr.Status.Failures = 0
	hr.Status.ObservedGeneration = hr.Generation
	_ = cl.Status().Update(ctx, hr)
	if rec != nil {
		rec.Event(hr, corev1.EventTypeNormal, v2.UpgradeSucceededReason, msg)
	}
}

// markFailure sets Ready=False and emits a warning event.
func markFailure(ctx context.Context, cl client.Client, rec record.EventRecorder, hr *v2.HelmRelease, err error) {
	msg := fmt.Sprintf("Helm upgrade failed for %s/%s: %s", hr.Namespace, hr.Name, err.Error())

	conditions.MarkFalse(hr, v2.ReleasedCondition, v2.UpgradeFailedReason, err.Error())
	conditions.MarkFalse(hr, fluxmeta.ReadyCondition, v2.UpgradeFailedReason, err.Error())

	hr.Status.Failures++
	hr.Status.ObservedGeneration = hr.Generation
	_ = cl.Status().Update(ctx, hr)
	if rec != nil {
		rec.Event(hr, corev1.EventTypeWarning, v2.UpgradeFailedReason, msg)
	}
}

// patchSuspend toggles spec.suspend using a merge-patch with a Flux field owner.
func patchSuspend(ctx context.Context, cl client.Client, ns, name string, val *bool) error {
	var payload []byte
	switch {
	case val == nil:
		payload = []byte(`{"spec":{"suspend":null}}`)
	case *val:
		payload = []byte(`{"spec":{"suspend":true}}`)
	default:
		payload = []byte(`{"spec":{"suspend":false}}`)
	}

	p := client.RawPatch(types.MergePatchType, payload)
	obj := &v2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	return cl.Patch(ctx, obj, p, client.FieldOwner("flux-client-side-apply"))
}

// discoverAPIVersions enumerates all apiVersions advertised by the cluster.
func discoverAPIVersions(rc *rest.Config) (hchart.VersionSet, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, err
	}
	grps, err := dc.ServerGroups()
	if err != nil {
		return nil, err
	}
	var vers []string
	for _, g := range grps.Groups {
		for _, v := range g.Versions {
			if g.Name == "" {
				vers = append(vers, v.Version) // core: v1
			} else {
				vers = append(vers, g.Name+"/"+v.Version)
			}
		}
	}
	return hchart.VersionSet(vers), nil
}

// loadExtraValues merges all files passed via -f/--values.
func loadExtraValues() (map[string]interface{}, error) {
	merged := map[string]interface{}{}
	for _, vf := range extraVals {
		data, err := os.ReadFile(vf)
		if err != nil {
			return nil, err
		}
		var mv map[string]interface{}
		if err := sigsyaml.Unmarshal(data, &mv); err != nil {
			return nil, err
		}
		if err := mergo.Merge(&merged, mv, mergo.WithOverride); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

func cmdCompletion() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate the autocompletion script for the specified shell",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unknown shell: %s", args[0])
			}
		},
	}
}

func completeNamespaces(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	var err error
	rc := restConfig()

	cl, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var nsList corev1.NamespaceList
	if err := cl.List(context.TODO(), &nsList); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var suggestions []string
	for _, ns := range nsList.Items {
		if strings.HasPrefix(ns.Name, toComplete) {
			suggestions = append(suggestions, ns.Name)
		}
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

func completeHelmReleases(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	rc := restConfig()

	cl, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	nsLocal := ns
	if nsLocal == "" {
		nsLocal, _, err = defaultNamespace()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
	}

	var list v2.HelmReleaseList
	if err := cl.List(context.TODO(), &list, &client.ListOptions{Namespace: nsLocal}); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var out []string
	for _, hr := range list.Items {
		if strings.HasPrefix(hr.Name, toComplete) {
			out = append(out, hr.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// cmdReconcile returns the `cozypkg reconcile` command.
func cmdReconcile() *cobra.Command {
	var (
		withSource bool
		force      bool
	)

	cmd := cmdFactory("reconcile", func(ctx context.Context, cl client.Client, _ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		// Check if HelmRelease is suspended
		if hr.Spec.Suspend {
			return fmt.Errorf("resource is suspended")
		}

		// ------------------------------------------------------------------ //
		// Clients & helpers                                                  //
		// ------------------------------------------------------------------ //
		var err error
		rc := restConfig()

		dyn, err := dynamic.NewForConfig(rc)
		if err != nil {
			return err
		}

		waitByWatch := func(gvr schema.GroupVersionResource, ns, name, field, want string) error {
			w, err := dyn.Resource(gvr).Namespace(ns).
				Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
			if err != nil {
				return err
			}
			for ev := range w.ResultChan() {
				u := ev.Object.(*unstructured.Unstructured)

				v, _, _ := unstructured.NestedString(u.Object, "status", field)
				if v == want {
					// — After match, check for failure
					conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
					if found {
						for _, c := range conds {
							m, ok := c.(map[string]interface{})
							if !ok {
								continue
							}
							if m["type"] == "Ready" {
								status := m["status"]
								if status == "True" {
									return nil
								}
								msg, _ := m["message"].(string)
								if msg == "" {
									msg = "unknown failure"
								}
								return fmt.Errorf("%s/%s: reconciliation failed: %s", gvr.Resource, name, msg)
							}
						}
					}
					return fmt.Errorf("%s/%s: reconciliation did not report Ready=True", gvr.Resource, name)
				}
			}
			return fmt.Errorf("%s/%s: timeout waiting for %s=%s", gvr.Resource, name, field, want)
		}

		// ------------------------------------------------------------------ //
		// 1. (optional) Source resource (HelmChart, ExternalArtifact, etc.) //
		// ------------------------------------------------------------------ //
		if withSource {
			var sourceGVR schema.GroupVersionResource
			var sourceNS, sourceName string
			var sourceReconciliationDone bool

			if hr.Spec.ChartRef != nil {
				// Reconcile the referenced source
				refNS := hr.Spec.ChartRef.Namespace
				if refNS == "" {
					refNS = hr.Namespace
				}
				refName := hr.Spec.ChartRef.Name
				sourceNS = refNS
				sourceName = refName

				switch hr.Spec.ChartRef.Kind {
				case "HelmChart":
					sourceGVR = schema.GroupVersionResource{
						Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts",
					}
				case "ExternalArtifact":
					// For ExternalArtifact, we need to find its source (GitRepository, OCIRepository, etc.)
					// through the ArtifactGenerator that created it
					artifactGen, err := findArtifactGeneratorForExternalArtifact(ctx, cl, refNS, refName)
					if err != nil {
						return fmt.Errorf("failed to find ArtifactGenerator for ExternalArtifact %s/%s: %w", refNS, refName, err)
					}

					// Found ArtifactGenerator, now find and reconcile its sources
					sources, found, _ := unstructured.NestedSlice(artifactGen.Object, "spec", "sources")
					if !found || len(sources) == 0 {
						return fmt.Errorf("ArtifactGenerator %s has no sources", artifactGen.GetName())
					}

					// Reconcile all sources
					for _, src := range sources {
						srcMap, ok := src.(map[string]interface{})
						if !ok {
							continue
						}
						kind, _, _ := unstructured.NestedString(srcMap, "kind")
						name, _, _ := unstructured.NestedString(srcMap, "name")
						srcNS, _, _ := unstructured.NestedString(srcMap, "namespace")
						if srcNS == "" {
							srcNS = refNS
						}

						// Determine GVR based on source kind
						var srcGVR schema.GroupVersionResource
						switch kind {
						case "GitRepository":
							srcGVR = schema.GroupVersionResource{
								Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories",
							}
						case "OCIRepository":
							srcGVR = schema.GroupVersionResource{
								Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories",
							}
						case "Bucket":
							srcGVR = schema.GroupVersionResource{
								Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "buckets",
							}
						default:
							continue
						}

						// Annotate and reconcile the source
						source := &unstructured.Unstructured{}
						source.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   srcGVR.Group,
							Version: srcGVR.Version,
							Kind:    kind,
						})
						if err := cl.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: name}, source); err != nil {
							return fmt.Errorf("%s %s/%s not found: %w", kind, srcNS, name, err)
						}
						// Check if source is suspended
						if suspended, found, _ := unstructured.NestedBool(source.Object, "spec", "suspend"); found && suspended {
							return fmt.Errorf("resource is suspended")
						}
						ts := time.Now().Format(time.RFC3339Nano)
						patch := client.MergeFrom(source.DeepCopy())
						ann := source.GetAnnotations()
						if ann == nil {
							ann = map[string]string{}
						}
						ann["reconcile.fluxcd.io/requestedAt"] = ts
						source.SetAnnotations(ann)
						if err := cl.Patch(ctx, source, patch); err != nil {
							return fmt.Errorf("patch %s: %w", kind, err)
						}
						fmt.Fprintf(os.Stderr, "✔ %s %s/%s annotated\n", kind, srcNS, name)

						fmt.Fprintf(os.Stderr, "◎ waiting for %s %s/%s reconciliation\n", kind, srcNS, name)
						if err := waitByWatch(srcGVR, srcNS, name, "lastHandledReconcileAt", ts); err != nil {
							return err
						}
						fmt.Fprintf(os.Stderr, "✔ %s %s/%s reconciled\n", kind, srcNS, name)
					}
					// After sources are reconciled, we're done with ExternalArtifact
					// Skip the rest of the source reconciliation logic for ExternalArtifact
					sourceReconciliationDone = true
				case "OCIRepository":
					sourceGVR = schema.GroupVersionResource{
						Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories",
					}
				default:
					return fmt.Errorf("unsupported chartRef kind for reconciliation: %s", hr.Spec.ChartRef.Kind)
				}
			} else if hr.Spec.Chart != nil {
				// Reconcile the generated HelmChart
				chartNS := hr.Spec.Chart.Spec.SourceRef.Namespace
				if chartNS == "" {
					chartNS = hr.Namespace
				}
				sourceNS = chartNS
				sourceName = fmt.Sprintf("%s-%s", hr.Namespace, hr.Name)
				sourceGVR = schema.GroupVersionResource{
					Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts",
				}
			} else {
				return fmt.Errorf("HelmRelease %s/%s has neither chart nor chartRef", hr.Namespace, hr.Name)
			}

			// Skip annotation if we already handled ExternalArtifact sources above
			if !sourceReconciliationDone {
				// Annotate the source resource
				source := &unstructured.Unstructured{}
				var kind string
				if hr.Spec.ChartRef != nil {
					kind = hr.Spec.ChartRef.Kind
				} else {
					kind = "HelmChart"
				}
				source.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   sourceGVR.Group,
					Version: sourceGVR.Version,
					Kind:    kind,
				})
				if err := cl.Get(ctx, types.NamespacedName{Namespace: sourceNS, Name: sourceName}, source); err != nil {
					return fmt.Errorf("%s %s/%s not found: %w", source.GetKind(), sourceNS, sourceName, err)
				}
				// Check if source is suspended
				if suspended, found, _ := unstructured.NestedBool(source.Object, "spec", "suspend"); found && suspended {
					return fmt.Errorf("resource is suspended")
				}
				ts := time.Now().Format(time.RFC3339Nano)
				patch := client.MergeFrom(source.DeepCopy())
				ann := source.GetAnnotations()
				if ann == nil {
					ann = map[string]string{}
				}
				ann["reconcile.fluxcd.io/requestedAt"] = ts
				source.SetAnnotations(ann)
				if err := cl.Patch(ctx, source, patch); err != nil {
					return fmt.Errorf("patch %s: %w", source.GetKind(), err)
				}
				fmt.Fprintf(os.Stderr, "✔ %s %s annotated\n", source.GetKind(), sourceName)

				fmt.Fprintf(os.Stderr, "◎ waiting for %s reconciliation\n", source.GetKind())
				if err := waitByWatch(sourceGVR, sourceNS, sourceName, "lastHandledReconcileAt", ts); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "✔ %s reconciled\n", source.GetKind())
			}
		}

		// ------------------------------------------------------------------ //
		// 2. HelmRelease                                                     //
		// ------------------------------------------------------------------ //
		ts := time.Now().Format(time.RFC3339Nano)
		ann := map[string]interface{}{
			"reconcile.fluxcd.io/requestedAt": ts,
		}
		if force {
			ann["reconcile.fluxcd.io/forceAt"] = ts
		}
		patch := map[string]interface{}{"metadata": map[string]interface{}{"annotations": ann}}
		pbytes, _ := json.Marshal(patch)
		if err := cl.Patch(ctx, hr,
			client.RawPatch(types.MergePatchType, pbytes)); err != nil {
			return fmt.Errorf("patch HelmRelease: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✔ HelmRelease %s annotated\n", hr.Name)

		fmt.Fprintln(os.Stderr, "◎ waiting for HelmRelease reconciliation")
		hrGVR := schema.GroupVersionResource{
			Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases",
		}
		if err := waitByWatch(hrGVR, hr.Namespace, hr.Name, "lastHandledReconcileAt", ts); err != nil {
			return err
		}
		if force {
			if err := waitByWatch(hrGVR, hr.Namespace, hr.Name, "lastHandledForceAt", ts); err != nil {
				return err
			}
		}
		fmt.Fprintln(os.Stderr, "✔ HelmRelease reconciled")
		return nil
	})

	cmd.Short = "Trigger HelmRelease reconciliation"
	cmd.Flags().BoolVar(&withSource, "with-source", false,
		"Reconcile the source HelmChart before the HelmRelease")
	cmd.Flags().BoolVar(&force, "force", false,
		"Force a one-off upgrade of the HelmRelease")
	return cmd
}

func discoverKubeVersion(rc *rest.Config) (hchart.KubeVersion, error) {
	if rc == nil {
		return hchart.KubeVersion{}, fmt.Errorf("renderManifests: kubeconfig not found – cannot discover cluster version")
	}
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return hchart.KubeVersion{}, err
	}
	info, err := dc.ServerVersion()
	if err != nil {
		return hchart.KubeVersion{}, err
	}
	return hchart.KubeVersion{
		Version: info.GitVersion,
		Major:   info.Major,
		Minor:   info.Minor,
	}, nil
}
