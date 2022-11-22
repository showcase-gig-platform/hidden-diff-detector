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
	kubeconfig  string
	extraConfig string
	filename    string
)

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
	cmd.PersistentFlags().StringVarP(&extraConfig, "extra-config", "e", "", "Path to extra config file.")

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
		klog.Fatal(err.Error())
	}

	us := splitYamlList(tmpBuf)
	rm, _ := restMapper(clientConfig)
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
	filtered := removeIgnoreFields(&uns)
	if e := writeFile(*filtered, dir); e != nil {
		return e
	}
	return nil
}

func writeOrigin(uns unstructured.Unstructured, rm meta.RESTMapper, client dynamic.Interface, dir string) error {
	orig, err := getOrigin(uns, rm, client)
	if err != nil {
		return err
	}

	filtered := removeIgnoreFields(orig)
	if err := writeFile(*filtered, dir); err != nil {
		return err
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

func removeIgnoreFields(orig *unstructured.Unstructured) *unstructured.Unstructured {
	unstructured.RemoveNestedField(orig.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(orig.Object, "metadata", "generation")
	unstructured.RemoveNestedField(orig.Object, "metadata", "annotations", "deployment.kubernetes.io/revision")
	unstructured.RemoveNestedField(orig.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	anot, _, _ := unstructured.NestedMap(orig.Object, "metadata", "annotations")
	if len(anot) == 0 {
		unstructured.RemoveNestedField(orig.Object, "metadata", "annotations")
	}
	lbls, _, _ := unstructured.NestedMap(orig.Object, "metadata", "labels")
	if len(lbls) == 0 {
		unstructured.RemoveNestedField(orig.Object, "metadata", "labels")
	}
	return orig
}

func writeFile(uns unstructured.Unstructured, dir string) error {
	b, err := kyaml.Marshal(uns.Object)
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
