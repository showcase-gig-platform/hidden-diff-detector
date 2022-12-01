package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
	"io"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/replace"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	kexec "k8s.io/utils/exec"
	"os"
	"path/filepath"
	kyaml "sigs.k8s.io/yaml"
	"strings"
)

const (
	tmpDir           = ".tmp"
	fromDirPrefix    = "LIVE"
	toDirPrefix      = "REPLACED"
	outputFormat     = "yaml"
	defaultNamespace = "default"
)

var (
	kubeconfig        string
	extraConfigPath   string
	filename          string
	loadedExtraConfig extraConfig
	ignoreGVKs        []schema.GroupVersionKind
)

type extraConfig struct {
	IgnoreResources []string    `yaml:"ignoreResources"`
	FieldFilter     interface{} `yaml:"fieldFilter"`
}

func main() {
	cmd := &cobra.Command{
		Use:     "use",
		Short:   "short",
		Long:    "long",
		Example: "example",
		Run: func(cmd *cobra.Command, args []string) {
			mainCmd()
		},
	}

	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	cmd.PersistentFlags().StringVarP(&kubeconfig, "kubeconfig", "k", "~/.kube/config", "Path to kubeconfig file.")
	cmd.PersistentFlags().StringVarP(&filename, "filename", "f", "", "Filename, directory, or URL to files contains the configuration to diff.")
	cmd.PersistentFlags().StringVarP(&extraConfigPath, "extra-config", "e", "", "Path to extra config file.")
	if err := cmd.Execute(); err != nil {
		klog.Fatal(err)
	}
}

func mainCmd() {
	fromDir, toDir := prepare()
	defer cleanup()

	if filename == "" {
		klog.Fatal("flag `--filename` or `-f` is required.")
	}
	kubeconfig = replaceHomedir(kubeconfig)

	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	client, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	rm, err := restMapper(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	loadedExtraConfig, err = loadExtraConfig()
	if err != nil {
		klog.Errorf("failed to load extra config: %s", err.Error())
	}

	ignoreGVKs = listIgnoreGVK(rm, loadedExtraConfig.IgnoreResources)

	var tmpBuf = &bytes.Buffer{}

	ioStreams := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    tmpBuf,
		ErrOut: os.Stderr,
	}
	cf := genericclioptions.NewConfigFlags(false)
	cf.KubeConfig = ptrString(kubeconfig)
	fact := cmdutil.NewFactory(cf)

	ro, err := newReplaceOptions(ioStreams, client, fact)

	if err := ro.Run(fact); err != nil {
		klog.Errorf("errors occurred when replacing resources : %s", err.Error())
	}

	us := splitYamlList(tmpBuf)
	for _, u := range us {
		if e := writeReplaced(u, toDir); e != nil {
			klog.Errorf("failed to write replaced yaml: %s", err.Error())
		}
		if e := writeOrigin(u, rm, client, fromDir); e != nil {
			klog.Errorf("failed to write origin yaml: %s", err.Error())
		}
	}

	if err := diff(fromDir, toDir); err != nil {
		klog.Fatal(err)
	}
}

// replaceOption各フィールドの詳細も把握していない（動けばヨシ状態）し、全体的に雑
func newReplaceOptions(stream genericclioptions.IOStreams, client dynamic.Interface, fact cmdutil.Factory) (*replace.ReplaceOptions, error) {
	var err error
	o := replace.NewReplaceOptions(stream)
	o.DeleteOptions, err = o.DeleteFlags.ToOptions(client, o.IOStreams)
	if err != nil {
		return nil, err
	}

	o.DryRunVerifier = resource.NewQueryParamVerifier(client, fact.OpenAPIGetter(), resource.QueryParamDryRun)
	o.FieldValidationVerifier = resource.NewQueryParamVerifier(client, fact.OpenAPIGetter(), resource.QueryParamDryRun)
	o.Builder = func() *resource.Builder {
		return fact.NewBuilder()
	}
	err = o.PrintFlags.Complete("%s (server dry run)")
	if err != nil {
		return nil, err
	}
	o.DryRunStrategy = cmdutil.DryRunServer

	recorder, err := o.RecordFlags.ToRecorder()
	if err != nil {
		return nil, err
	}
	o.Recorder = recorder

	o.PrintFlags.OutputFormat = ptrString(outputFormat)
	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return nil, err
	}
	o.PrintObj = func(obj runtime.Object) error {
		return printer.PrintObj(obj, o.Out)
	}

	o.DeleteOptions.FilenameOptions.Filenames = []string{filename}
	o.Namespace = defaultNamespace // どうやらmanifestにnamespaceがない場合のデフォルト値っぽい（要確認）
	o.DeleteOptions.ForceDeletion = false
	return o, nil
}

func ptrString(str string) *string {
	return &str
}

func restMapper(c *rest.Config) (meta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(c)
	if err != nil {
		return nil, err
	}
	gr, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(gr)

	return mapper, nil
}

func diff(from, to string) error {
	cmd := kexec.New().Command("diff", "-u", "-N", from, to)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)
	err := cmd.Run()
	if err != nil && stderr.String() != "" {
		return fmt.Errorf("cmd execution exit with error: `%s`, stderr output: %v", err.Error(), stderr.String())
	}
	fmt.Println(stdout.String())
	return nil
}

// ---で区切られたyamlをdecodeした配列にする
func splitYamlList(buf *bytes.Buffer) []unstructured.Unstructured {
	var us []unstructured.Unstructured
	decoder := yaml.NewDecoder(buf)
	for {
		var u unstructured.Unstructured

		if err := decoder.Decode(&u.Object); err != nil {
			if err != io.EOF {
				klog.Error(err)
			}
			break
		}
		us = append(us, u)
	}
	return us
}

func writeReplaced(uns unstructured.Unstructured, dir string) error {
	if matchGroupKind(uns, ignoreGVKs) {
		return nil
	}
	filtered := removeIgnoreFields(&uns)
	fin := customFieldFilter(filtered.Object, loadedExtraConfig.FieldFilter)
	if e := writeFile(uns, fin, dir); e != nil {
		return e
	}
	return nil
}

func writeOrigin(uns unstructured.Unstructured, rm meta.RESTMapper, client dynamic.Interface, dir string) error {
	if matchGroupKind(uns, ignoreGVKs) {
		return nil
	}
	orig, err := getOrigin(uns, rm, client)
	if err != nil {
		return err
	}

	filtered := removeIgnoreFields(orig)
	fin := customFieldFilter(filtered.Object, loadedExtraConfig.FieldFilter)
	if e := writeFile(uns, fin, dir); e != nil {
		return e
	}
	return nil
}

func getOrigin(uns unstructured.Unstructured, rm meta.RESTMapper, client dynamic.Interface) (*unstructured.Unstructured, error) {
	gv := strings.Split(uns.GetAPIVersion(), "/")
	if len(gv) == 1 {
		gv = append([]string{""}, gv...)
	}
	g := gv[0]
	v := gv[1]

	r, err := rm.RESTMapping(schema.GroupKind{
		Group: g,
		Kind:  uns.GetKind(),
	}, v)
	if err != nil {
		return nil, err
	}
	gvr := r.Resource
	orig, err := client.Resource(gvr).Namespace(uns.GetNamespace()).Get(context.TODO(), uns.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return orig, nil
}

// replaceによりどうしても差分が出ちゃうものを削除
func removeIgnoreFields(orig *unstructured.Unstructured) *unstructured.Unstructured {
	unstructured.RemoveNestedField(orig.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(orig.Object, "metadata", "generation")
	unstructured.RemoveNestedField(orig.Object, "metadata", "annotations", "deployment.kubernetes.io/revision")
	unstructured.RemoveNestedField(orig.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	anot, _, _ := unstructured.NestedMap(orig.Object, "metadata", "annotations")
	if len(anot) == 0 {
		unstructured.RemoveNestedField(orig.Object, "metadata", "annotations")
	}
	if orig.GetKind() == "ServiceAccount" {
		unstructured.RemoveNestedField(orig.Object, "secrets")
	}
	return orig
}

func writeFile(uns unstructured.Unstructured, data interface{}, dir string) error {
	b, err := kyaml.Marshal(data)
	if err != nil {
		return err
	}

	fn := filepath.Join(dir, strings.ReplaceAll(uns.GetAPIVersion(), "/", ".")+"."+uns.GetKind()+"."+uns.GetNamespace()+"."+uns.GetName())

	file, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			klog.Error(err)
		}
	}()
	w := bufio.NewWriter(file)
	if _, e := w.Write(b); e != nil {
		return e
	}
	if e := w.Flush(); e != nil {
		return e
	}
	return nil
}

func prepare() (string, string) {
	cleanup()
	err := os.Mkdir(tmpDir, os.ModePerm)
	if err != nil {
		klog.Fatal(err)
	}
	from, err := os.MkdirTemp(tmpDir, fromDirPrefix+"-")
	if err != nil {
		klog.Fatal(err)
	}
	to, err := os.MkdirTemp(tmpDir, toDirPrefix+"-")
	if err != nil {
		klog.Fatal(err)
	}
	return from, to
}

func cleanup() {
	if err := os.RemoveAll(tmpDir); err != nil {
		klog.Errorf("failed to remove tmp directory: %s", err)
	}
}

func replaceHomedir(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		klog.Fatal(err)
	}
	return strings.ReplaceAll(path, "~", home)
}

func loadExtraConfig() (extraConfig, error) {
	var cfg extraConfig
	if extraConfigPath == "" {
		klog.Infoln("no setting for extra config")
		return cfg, nil
	}
	buf, err := os.ReadFile(extraConfigPath)
	if err != nil {
		return cfg, fmt.Errorf("failed to load extra config file : %s", err.Error())
	}
	if e := yaml.Unmarshal(buf, &cfg); e != nil {
		return cfg, fmt.Errorf("failed to ummarshal extra config yaml : %s", e.Error())
	}
	cfg, e := parseFieldFilter(cfg)
	if e != nil {
		return cfg, fmt.Errorf("failed to parse field filter : %s", e.Error())
	}
	return cfg, nil
}

func parseFieldFilter(cfg extraConfig) (extraConfig, error) {
	switch icfg := cfg.FieldFilter.(type) {
	case string:
		var y interface{}
		if e := yaml.Unmarshal([]byte(icfg), &y); e != nil {
			return cfg, fmt.Errorf("failed to unmarshal fieldFilter : ,%s", e.Error())
		}
		cfg.FieldFilter = y
	default:
		//
	}
	return cfg, nil
}

func customFieldFilter(isrc, itgt interface{}) interface{} {
	if itgt == nil {
		return isrc
	}
	switch isrc.(type) {
	case map[string]interface{}:
		result := map[string]interface{}{}
		src, sok := isrc.(map[string]interface{})
		tgt, tok := itgt.(map[string]interface{})
		if !(sok && tok) {
			return isrc
		}
		for sk, s := range src {
			remain := true
			for tk, t := range tgt {
				if sk == tk {
					if empty(t) || t == nil {
						remain = false
						break
					} else {
						s = customFieldFilter(s, t)
					}
				}
			}
			if remain && !empty(s) {
				result[sk] = s
			}
		}
		return result
	case []interface{}:
		var result []interface{}
		src, sok := isrc.([]interface{})
		tgt, tok := itgt.([]interface{})
		if !(sok && tok) {
			return isrc
		}
		for _, s := range src {
			remain := true
			for _, t := range tgt {
				mat, nest := match(s, t)
				if mat {
					remain = false
					break
				} else {
					if nest {
						s = customFieldFilter(s, t)
					}
				}
			}
			if remain == true {
				result = append(result, s)
			}
		}
		return result
	default:
		return isrc
	}
}

func match(isrc, itgt interface{}) (bool, bool) {
	switch tgt := itgt.(type) {
	case map[string]interface{}:
		if len(tgt) != 1 { // 複数要素のマッチは未対応
			return false, true
		}
		var tkey string
		var tval interface{}
		for k, iv := range tgt {
			tkey = k
			switch t := iv.(type) {
			case map[string]interface{}, []interface{}:
				return false, true // 更にnestがある場合はmatchを評価しない
			default:
				tval = t
			}
		}
		src, ok := isrc.(map[string]interface{})
		if ok {
			for k, v := range src {
				switch v.(type) {
				case map[string]interface{}, []interface{}:
					continue
				default:
					if tkey == k && tval == v {
						return true, false
					}
				}
			}
		}
		return false, false
	case []interface{}:
		return false, false // 現状ここには来ないはず（arrayが直接nestになるケースがあれば来る）
	default:
		if isrc == itgt {
			return true, false
		}
		return false, false
	}
}

func empty(i interface{}) bool {
	m, mok := i.(map[string]interface{})
	if mok {
		if len(m) == 0 {
			return true
		}
	}
	s, sok := i.([]interface{})
	if sok {
		if len(s) == 0 {
			return true
		}
	}
	return false
}

func listIgnoreGVK(rm meta.RESTMapper, resources []string) []schema.GroupVersionKind {
	var result []schema.GroupVersionKind
	for _, r := range resources {
		if gvk, err := rm.KindFor(schema.GroupVersionResource{
			Group:    "",
			Version:  "",
			Resource: r,
		}); err != nil {
			klog.Errorf("failed to get GVK from resource : %s", err.Error())
		} else {
			result = append(result, gvk)
		}
	}
	return result
}

func matchGroupKind(uns unstructured.Unstructured, gvks []schema.GroupVersionKind) bool {
	for _, gvk := range gvks {
		gv := strings.Split(uns.GetAPIVersion(), "/")
		if len(gv) == 1 {
			gv = append([]string{""}, gv...)
		}
		g := gv[0]

		if g == gvk.Group && uns.GetKind() == gvk.Kind {
			return true
		}
	}
	return false
}
