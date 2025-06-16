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
	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"github.com/databus23/helm-diff/v3/diff"
	"github.com/databus23/helm-diff/v3/manifest"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Global CLI flags.
var Version = "dev"

var (
	kubeCfgPath string
	ns          string
	chDir       string
	plain       bool     // render without talking to the API server
	showOnly    []string // template globs for `cozypkg show`
	extraVals   []string // additional -f/--values files
)

func init() {
	_ = v2.AddToScheme(clientsetscheme.Scheme)
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

	root.PersistentFlags().StringVar(&kubeCfgPath, "kubeconfig", "", "Path to kubeconfig")
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

// restConfig builds a *rest.Config from the --kubeconfig flag or $KUBECONFIG.
func restConfig() (*rest.Config, error) {
	if path := resolvedKubeconfigPath(); path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}

// helmCfg returns an initialised Helm configuration bound to a namespace.
func helmCfg(rc *rest.Config, namespace string) (*helmaction.Configuration, *helmcfg.EnvSettings, error) {
	env := helmcfg.New()
	if kubeCfgPath != "" {
		env.KubeConfig = kubeCfgPath
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

// mergedValues merges valuesFiles and inline .spec.values in the given HelmRelease.
func mergedValues(hr *v2.HelmRelease, chartDir string) (map[string]interface{}, error) {
	vals := map[string]interface{}{}

	for _, vf := range hr.Spec.Chart.Spec.ValuesFiles {
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
func upgradeRelease(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string, vals map[string]interface{}) error {
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
		if !plain {
			inst.PostRenderer = &fluxPostRenderer{name: relName, ns: namespace}
		}
		_, err = inst.Run(ch, vals)
		return err
	}

	up := helmaction.NewUpgrade(cfg)
	up.Namespace = namespace
	up.Install = true // informative only
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
	inst.PostRenderer = &fluxPostRenderer{name: hr.Name, ns: hr.Namespace}

	rc, err := restConfig()
	if err != nil {
		return "", err
	}
	kubeVer, err := discoverKubeVersion(rc)
	if err != nil {
		return "", err
	}
	inst.KubeVersion = &kubeVer

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

	_ = diff.Manifests(curSpecs, newSpecs, &diff.Options{OutputContext: -1}, &buf)
	return buf.String(), nil
}

// runFn is a helper signature passed to cmdFactory.
type runFn func(*helmaction.Configuration, *v2.HelmRelease, string) error

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

			// Offline mode: create a minimal stub.
			if plain {
				if ns == "" {
					ns = "default"
				}
				stub := &v2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: relName, Namespace: ns}}
				rc, _ := restConfig()
				cfg, _, err := helmCfg(rc, ns)
				if err != nil {
					return err
				}
				return fn(cfg, stub, ".")
			}

			rc, err := restConfig()
			if err != nil {
				return err
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

			cl, err := ctrlclient.New(rc, ctrlclient.Options{})
			if err != nil {
				return err
			}

			var hr v2.HelmRelease
			if err := cl.Get(context.TODO(), ctrlclient.ObjectKey{Namespace: ns, Name: relName}, &hr); err != nil {
				return err
			}

			return fn(cfg, &hr, ".")
		},
	}
	cmd.ValidArgsFunction = completeHelmReleases
	return cmd
}

// cmdShow returns the `cozypkg show` command.
func cmdShow() *cobra.Command {
	cmd := cmdFactory("show", func(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		var vals map[string]interface{}
		if plain {
			vals = map[string]interface{}{}
		} else {
			var err error
			vals, err = mergedValues(hr, chartDir)
			if err != nil {
				return err
			}
		}

		if add, err := loadExtraValues(); err != nil {
			return err
		} else if err := mergo.Merge(&vals, add, mergo.WithOverride); err != nil {
			return err
		}

		rc, _ := restConfig()
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
	cmd := cmdFactory("apply", func(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		if plain && autoResume {
			return fmt.Errorf("--resume may not be used with --plain")
		}
		ctx := context.Background()

		rc, _ := restConfig()
		cl, _ := ctrlclient.New(rc, ctrlclient.Options{})

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
			vals, err = mergedValues(hr, chartDir)
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
			cfgDigest := fluxchartutil.DigestValues(digest.Canonical, vals).String()
			chartVer := hr.Spec.Chart.Spec.Version

			hr.Status.LastAttemptedGeneration = hr.Generation
			hr.Status.LastAttemptedRevision = chartVer
			hr.Status.LastAttemptedConfigDigest = cfgDigest
			hr.Status.LastAttemptedReleaseAction = v2.ReleaseActionUpgrade
			_ = cl.Status().Update(ctx, hr)
		}

		if err := upgradeRelease(cfg, hr, chartDir, vals); err != nil {
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
	return cmd
}

// cmdDiff returns the `cozypkg diff` command.
func cmdDiff() *cobra.Command {
	cmd := cmdFactory("diff", func(cfg *helmaction.Configuration, hr *v2.HelmRelease, chartDir string) error {
		var vals map[string]interface{}
		if plain {
			vals = map[string]interface{}{}
		} else {
			var err error
			vals, err = mergedValues(hr, chartDir)
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
	cmd := cmdFactory("suspend", func(_ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		rc, _ := restConfig()
		cl, _ := ctrlclient.New(rc, ctrlclient.Options{})
		return patchSuspend(context.Background(), cl, hr.Namespace, hr.Name, pointer.Bool(true))
	})
	cmd.Short = "Suspend Flux HelmRelease"
	return cmd
}

// cmdResume returns the `cozypkg resume` command.
func cmdResume() *cobra.Command {
	cmd := cmdFactory("resume", func(_ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		rc, _ := restConfig()
		cl, _ := ctrlclient.New(rc, ctrlclient.Options{})
		return patchSuspend(context.Background(), cl, hr.Namespace, hr.Name, nil)
	})
	cmd.Short = "Resume Flux HelmRelease"
	return cmd
}

// cmdDelete returns the `cozypkg delete` command.
func cmdDelete() *cobra.Command {
	cmd := cmdFactory("delete", func(cfg *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		un := helmaction.NewUninstall(cfg)
		_, err := un.Run(hr.Name)
		return err
	})
	cmd.Short = "Uninstall the Helm release"
	return cmd
}

// defaultNamespace returns the namespace from the kubeconfig or "default".
func defaultNamespace() (string, bool, error) {
	path := resolvedKubeconfigPath()
	if path != "" {
		loading := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{})
		ns, overridden, err := cc.Namespace()
		if err != nil {
			return "default", false, nil
		}
		return ns, overridden, nil
	}

	// Try reading namespace from in-cluster location
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return strings.TrimSpace(string(data)), false, nil
	}

	// Fallback
	return "default", false, nil
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

	rc, err := restConfig()
	if err != nil {
		return err
	}

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
func newHistoryEntry(hr *v2.HelmRelease, chartVersion, cfgDigest string) *v2.Snapshot {
	return &v2.Snapshot{
		Name:          hr.Spec.Chart.Spec.Chart,
		Namespace:     hr.Namespace,
		Version:       1,
		ChartName:     hr.Spec.Chart.Spec.Chart,
		ChartVersion:  chartVersion,
		ConfigDigest:  cfgDigest,
		FirstDeployed: metav1.Now(),
		LastDeployed:  metav1.Now(),
		Status:        "deployed",
	}
}

// markSuccess sets Ready=True and emits a normal event.
func markSuccess(ctx context.Context, cl ctrlclient.Client, rec record.EventRecorder, hr *v2.HelmRelease, chartVer, cfgDigest string) {
	msg := fmt.Sprintf("Helm upgrade succeeded for %s/%s with chart %s@%s", hr.Namespace, hr.Name, hr.Spec.Chart.Spec.Chart, chartVer)

	conditions.MarkTrue(hr, v2.ReleasedCondition, v2.UpgradeSucceededReason, msg)
	conditions.MarkTrue(hr, fluxmeta.ReadyCondition, v2.UpgradeSucceededReason, msg)

	hr.Status.History = append(hr.Status.History, newHistoryEntry(hr, chartVer, cfgDigest))
	hr.Status.Failures = 0
	hr.Status.ObservedGeneration = hr.Generation
	_ = cl.Status().Update(ctx, hr)
	if rec != nil {
		rec.Event(hr, corev1.EventTypeNormal, v2.UpgradeSucceededReason, msg)
	}
}

// markFailure sets Ready=False and emits a warning event.
func markFailure(ctx context.Context, cl ctrlclient.Client, rec record.EventRecorder, hr *v2.HelmRelease, err error) {
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
func patchSuspend(ctx context.Context, cl ctrlclient.Client, ns, name string, val *bool) error {
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
	rc, err := restConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	cl, err := ctrlclient.New(rc, ctrlclient.Options{})
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
	rc, err := restConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	cl, err := ctrlclient.New(rc, ctrlclient.Options{})
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

	cmd := cmdFactory("reconcile", func(_ *helmaction.Configuration, hr *v2.HelmRelease, _ string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// ------------------------------------------------------------------ //
		// Clients & helpers                                                  //
		// ------------------------------------------------------------------ //
		rc, err := restConfig()
		if err != nil {
			return err
		}
		cl, err := ctrlclient.New(rc, ctrlclient.Options{})
		if err != nil {
			return err
		}
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
		// 1. (optional) HelmChart                                            //
		// ------------------------------------------------------------------ //
		if withSource {
			chartNS := hr.Spec.Chart.Spec.SourceRef.Namespace
			if chartNS == "" {
				chartNS = hr.Namespace
			}
			chartName := fmt.Sprintf("%s-%s", hr.Namespace, hr.Name)
			chartGVR := schema.GroupVersionResource{
				Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts",
			}

			// annotate
			chart := &unstructured.Unstructured{}
			chart.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "source.toolkit.fluxcd.io",
				Version: "v1",
				Kind:    "HelmChart",
			})
			if err := cl.Get(ctx, types.NamespacedName{Namespace: chartNS, Name: chartName}, chart); err != nil {
				return fmt.Errorf("HelmChart %s/%s not found: %w", chartNS, chartName, err)
			}
			ts := time.Now().Format(time.RFC3339Nano)
			patch := client.MergeFrom(chart.DeepCopy())
			ann := chart.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["reconcile.fluxcd.io/requestedAt"] = ts
			chart.SetAnnotations(ann)
			if err := cl.Patch(ctx, chart, patch); err != nil {
				return fmt.Errorf("patch HelmChart: %w", err)
			}
			fmt.Fprintf(os.Stderr, "✔ HelmChart %s annotated\n", chartName)

			fmt.Fprintln(os.Stderr, "◎ waiting for HelmChart reconciliation")
			if err := waitByWatch(chartGVR, chartNS, chartName, "lastHandledReconcileAt", ts); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "✔ HelmChart reconciled")
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
			ctrlclient.RawPatch(types.MergePatchType, pbytes)); err != nil {
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

func resolvedKubeconfigPath() string {
	if kubeCfgPath != "" {
		return kubeCfgPath
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cfg := filepath.Join(home, ".kube", "config")
	if _, err := os.Stat(cfg); err == nil {
		return cfg
	}
	return ""
}
