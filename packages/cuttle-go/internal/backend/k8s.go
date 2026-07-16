package backend

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/config"
)

// chartPath is the Helm chart the k8s backend installs. Relative to the repo
// root; a packaged binary would embed it (see plan 8.9).
const chartPath = "ops/helm/cuttle"

const instanceSelector = "app.kubernetes.io/instance="

// defaultRelease is the Helm release name when a k8s context omits `release`.
const defaultRelease = "cuttle"

// K8s runs the browser as a Helm-managed Deployment in a cluster, reached via
// kubectl port-forward. It shells out to kubectl/helm and inherits the user's
// kube context (and thus their routing) with zero cuttle-specific setup.
type K8s struct {
	runner      Runner
	namespace   string
	release     string
	kubeContext string
	ctx         config.Context
}

func newK8s(ctx config.Context, r Runner) *K8s {
	release := ctx.Release
	if release == "" {
		release = defaultRelease
	}
	namespace := ctx.Namespace
	if namespace == "" {
		namespace = "default"
	}
	return &K8s{runner: r, namespace: namespace, release: release, kubeContext: ctx.KubeContext, ctx: ctx}
}

func (k *K8s) check() error {
	if err := requireExe(k.runner, "kubectl", "install kubectl and configure a cluster context first."); err != nil {
		return err
	}
	return requireExe(k.runner, "helm", "install Helm first.")
}

// kubectlArgs threads the kube context and namespace onto every kubectl call.
func (k *K8s) kubectlArgs(args ...string) []string {
	out := []string{}
	if k.kubeContext != "" {
		out = append(out, "--context", k.kubeContext)
	}
	out = append(out, "-n", k.namespace)
	return append(out, args...)
}

// helmArgs threads the kube context and namespace onto every helm call.
func (k *K8s) helmArgs(args ...string) []string {
	out := []string{}
	if k.kubeContext != "" {
		out = append(out, "--kube-context", k.kubeContext)
	}
	out = append(out, "--namespace", k.namespace)
	return append(out, args...)
}

func (k *K8s) State(ctx context.Context) (State, error) {
	if err := k.check(); err != nil {
		return "", err
	}
	res, err := k.runner.Output(ctx, "kubectl", k.kubectlArgs(
		"get", "pod", "-l", instanceSelector+k.release, "-o", "jsonpath={.items[*].status.phase}",
	)...)
	if err != nil {
		return "", err
	}
	phases := strings.TrimSpace(res.Stdout)
	if res.Code != 0 || phases == "" {
		return StateAbsent, nil
	}
	if strings.Contains(phases, "Running") {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func (k *K8s) Start(ctx context.Context, opts StartOpts) error {
	if err := k.check(); err != nil {
		return err
	}
	setArgs := k.installSets(opts)
	args := k.helmArgs(append([]string{"upgrade", helmInstall, k.release, chartPath, "--create-namespace"}, setArgs...)...)
	res, err := k.runner.Output(ctx, "helm", args...)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("helm upgrade failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
	}
	return nil
}

// installSets builds the --set flags for the chart from the context config and
// start options. Keys are dotted paths (a dot separates path segments), so only
// dynamic segments (map keys) are escaped; values only need comma-escaping.
// Map/list entries are emitted in a stable (sorted / indexed) order so the argv
// is deterministic.
func (k *K8s) installSets(opts StartOpts) []string {
	var sets []string
	set := func(key, v string) { sets = append(sets, "--set", key+"="+escapeHelmValue(v)) }
	setStr := func(key, v string) { sets = append(sets, "--set-string", key+"="+escapeHelmValue(v)) }

	set("replicaCount", "1")
	if opts.Image != "" {
		if _, tag, ok := strings.Cut(opts.Image, ":"); ok {
			setStr("image.tag", tag)
		}
	}
	if opts.Proxy != "" {
		setStr("proxy", opts.Proxy)
	}
	storage := opts.Storage
	if storage == "" {
		storage = config.StorageLocal
	}
	setStr("profileStorage", storage)

	for _, key := range sortedKeys(k.ctx.NodeSelector) {
		setStr("nodeSelector."+escapeHelmSegment(key), k.ctx.NodeSelector[key])
	}
	for i, tol := range k.ctx.Tolerations {
		prefix := fmt.Sprintf("tolerations[%d].", i)
		if tol.Key != "" {
			setStr(prefix+"key", tol.Key)
		}
		if tol.Operator != "" {
			setStr(prefix+"operator", tol.Operator)
		}
		if tol.Value != "" {
			setStr(prefix+"value", tol.Value)
		}
		if tol.Effect != "" {
			setStr(prefix+"effect", tol.Effect)
		}
	}
	if k.ctx.Resources != nil {
		for _, key := range sortedKeys(k.ctx.Resources.Requests) {
			setStr("resources.requests."+escapeHelmSegment(key), k.ctx.Resources.Requests[key])
		}
		for _, key := range sortedKeys(k.ctx.Resources.Limits) {
			setStr("resources.limits."+escapeHelmSegment(key), k.ctx.Resources.Limits[key])
		}
	}
	return sets
}

func (k *K8s) Stop(ctx context.Context, purge bool) error {
	if err := k.check(); err != nil {
		return err
	}
	if !purge {
		args := k.helmArgs("upgrade", helmInstall, k.release, chartPath, "--reuse-values", "--set", "replicaCount=0")
		res, err := k.runner.Output(ctx, "helm", args...)
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("helm scale-down failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
		}
		return nil
	}

	res, err := k.runner.Output(ctx, "helm", k.helmArgs("uninstall", k.release)...)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("helm uninstall failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
	}
	res, err = k.runner.Output(ctx, "kubectl", k.kubectlArgs("delete", "pvc", "-l", instanceSelector+k.release)...)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("kubectl delete pvc failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
	}
	return nil
}

// Reach opens a kubectl port-forward onto auto-picked free local ports so a k8s
// attach never collides with a local container already on 9222.
func (k *K8s) Reach(ctx context.Context) (Endpoint, func(), error) {
	if err := k.check(); err != nil {
		return Endpoint{}, nil, err
	}
	cdpLocal, err := freePort()
	if err != nil {
		return Endpoint{}, nil, err
	}
	vncLocal, err := freePort()
	if err != nil {
		return Endpoint{}, nil, err
	}
	args := k.kubectlArgs(
		"port-forward", "svc/"+k.release,
		portStr(cdpLocal)+":"+containerCDPPort,
		portStr(vncLocal)+":"+containerVNCPort,
	)
	proc, err := k.runner.Start(ctx, "kubectl", args...)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("starting port-forward: %w", err)
	}
	ep := Endpoint{CDPHost: loopbackHost, CDPPort: cdpLocal, VNCHost: loopbackHost, VNCPort: vncLocal}
	return ep, func() { _ = proc.Stop() }, nil
}

// escapeHelmSegment escapes a single --set key path segment: dots (which would
// otherwise split the path) and commas, so a map key like "glim.sh/browser"
// stays one segment.
func escapeHelmSegment(s string) string {
	return strings.NewReplacer(".", `\.`, ",", `\,`).Replace(s)
}

// escapeHelmValue escapes only what separates --set values: commas. Dots,
// slashes, and colons are literal in values, so a credentialed proxy URL passes
// through intact.
func escapeHelmValue(s string) string {
	return strings.ReplaceAll(s, ",", `\,`)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
