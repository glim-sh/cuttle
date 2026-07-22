package backend

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/glim-sh/cuttle/internal/config"
)

// chartPath is the Helm chart the k8s backend installs. Relative to the repo
// root; a packaged binary would embed it.
const chartPath = "ops/helm/cuttle"

const instanceSelector = "app.kubernetes.io/instance="

// defaultRelease is the Helm release name when a k8s context omits `release`.
const defaultRelease = "cuttle"

// K8s runs the browser as a Helm-managed Deployment in a cluster, reached via
// kubectl port-forward. It shells out to kubectl/helm and inherits the user's
// kube context (and thus their routing) with zero cuttle-specific setup.
type K8s struct {
	runner        Runner
	namespace     string
	release       string
	ctx           config.Context
	tunnelContext string // resolved context name; standing-tunnel pidfile identity
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
	return &K8s{runner: r, namespace: namespace, release: release, ctx: ctx}
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
	if k.ctx.KubeContext != "" {
		out = append(out, "--context", k.ctx.KubeContext)
	}
	out = append(out, "-n", k.namespace)
	return append(out, args...)
}

// helmArgs threads the kube context and namespace onto every helm call.
func (k *K8s) helmArgs(args ...string) []string {
	out := []string{}
	if k.ctx.KubeContext != "" {
		out = append(out, "--kube-context", k.ctx.KubeContext)
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
	// --purge-profile resets the durable profile (resolves #27's reset controls).
	// The profile PVC is RWO and held by the running pod, so release it first: a
	// `helm uninstall` tears down the Deployment (the PVC survives via its keep
	// resource-policy), then the PVC is deleted, then the install below recreates
	// a fresh one. This is the k8s analogue of removing the named Docker volume.
	if opts.PurgeProfile {
		if err := k.Stop(ctx, true); err != nil {
			return err
		}
	}
	if opts.Persistent() {
		if err := k.ensureDefaultStorageClass(ctx); err != nil {
			return err
		}
	}
	setArgs := k.installSets(opts)
	args := k.helmArgs(append([]string{"upgrade", helmInstall, k.release, chartPath, "--create-namespace"}, setArgs...)...)
	return runOK(ctx, k.runner, "helm upgrade", "helm", args...)
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
	storage := config.StorageLocal
	if opts.Persistent() {
		storage = config.StorageRemote
	}
	setStr("profileStorage", storage)
	if opts.IdleTimeout != "" {
		setStr("idleTimeout", opts.IdleTimeout)
	}

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
		return runOK(ctx, k.runner, "helm scale-down", "helm", args...)
	}

	// --ignore-not-found makes purge idempotent: `up --purge-profile` / `down
	// --purge` on a context with no release (or an already-torn-down one) is a
	// clean no-op instead of a helm error, matching the docker volumeRm path.
	if err := runOK(ctx, k.runner, "helm uninstall", "helm", k.helmArgs("uninstall", k.release, "--ignore-not-found")...); err != nil {
		return err
	}
	return k.deletePVC(ctx)
}

// deletePVC removes this release's durable profile PVC, idempotently: kubectl
// --ignore-not-found makes a no-match delete a clean no-op, so purge never errors
// on an absent PVC.
func (k *K8s) deletePVC(ctx context.Context) error {
	return runOK(ctx, k.runner, "kubectl delete pvc", "kubectl",
		k.kubectlArgs("delete", "pvc", "-l", instanceSelector+k.release, "--ignore-not-found")...)
}

// ensureDefaultStorageClass fails fast when a persistent (remote) install would
// provision a PVC on a cluster with no default StorageClass - otherwise the PVC
// stays Pending and the pod never starts, with no obvious cause. Fail-open: if
// the cluster cannot be queried, or the annotation output is empty/ambiguous, it
// proceeds (no worse off than before this check). A default class is detected by
// the is-default-class annotation on any class, covering the stable and beta
// annotation keys alike.
func (k *K8s) ensureDefaultStorageClass(ctx context.Context) error {
	// A PVC already exists (a re-bind / restart, not a fresh provision): it is
	// bound, so there is no Pending risk to warn about - skip the check. This also
	// avoids a false positive if the cluster's default class was removed after the
	// PVC was first provisioned.
	if pvc, err := k.runner.Output(ctx, "kubectl",
		k.kubectlArgs("get", "pvc", "-l", instanceSelector+k.release, "-o", "name")...); err == nil && strings.TrimSpace(pvc.Stdout) != "" {
		return nil
	}
	res, err := k.runner.Output(ctx, "kubectl",
		k.kubectlArgs("get", "storageclass", "-o", "jsonpath={.items[*].metadata.annotations}")...)
	if err != nil || res.Code != 0 {
		return nil //nolint:nilerr // fail-open: an unqueryable cluster must not block the install
	}
	out := strings.TrimSpace(res.Stdout)
	// Current kubectl renders the annotations map as JSON (`"is-default-class":"true"`);
	// older versions used Go's map syntax (`is-default-class:true`). Accept either, or
	// a cluster that HAS a default class is misread as having none and the install is
	// wrongly blocked. The match covers the stable and beta annotation keys alike
	// (both end in is-default-class).
	if out == "" || strings.Contains(out, `is-default-class":"true"`) || strings.Contains(out, "is-default-class:true") {
		return nil
	}
	return fmt.Errorf("%s: the cluster has no default StorageClass, so the persistent profile PVC would stay Pending - set persistence.storageClass in the chart values, or run `cuttle up --ephemeral`", k.release) //nolint:err113
}

// PurgeProfileVolume deletes the durable profile PVC, the k8s analogue of
// removing the named Docker volume. `cuttle purge-profile` uninstalls the release
// first (releasing the RWO claim), so this deletes a now-unbound PVC; a lingering
// PVC from a prior install is removed too.
func (k *K8s) PurgeProfileVolume(ctx context.Context) error {
	if err := k.check(); err != nil {
		return err
	}
	return k.deletePVC(ctx)
}

// Reach opens a kubectl port-forward. cdpPort/vncPort pin the local ports (so a
// held `cuttle connect` forward is deterministic and a driver can attach to it); 0
// auto-picks free ports for the ephemeral status/login forwards, which then
// never collide with a local container already on 9222.
func (k *K8s) Reach(ctx context.Context, cdpPort, vncPort int) (Endpoint, func(), error) {
	if err := k.check(); err != nil {
		return Endpoint{}, nil, err
	}
	cdpLocal, err := chooseLocalPort(cdpPort)
	if err != nil {
		return Endpoint{}, nil, err
	}
	vncLocal, err := chooseLocalPort(vncPort)
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

// EnsureTunnel establishes (or reuses) a detached `kubectl port-forward` on the
// fixed cdp/vnc ports that outlives the CLI. The forward dies when the pod
// restarts; status re-establishes it on the next health-check.
func (k *K8s) EnsureTunnel(ctx context.Context, cdpPort, vncPort int) (Endpoint, error) {
	if err := k.check(); err != nil {
		return Endpoint{}, err
	}
	args := k.kubectlArgs(
		"port-forward", "svc/"+k.release,
		portStr(cdpPort)+":"+containerCDPPort,
		portStr(vncPort)+":"+containerVNCPort,
	)
	return ensureTunnel(ctx, tunnelSpec{context: k.tunnelContext, name: "kubectl", args: args, cdpPort: cdpPort, vncPort: vncPort})
}

func (k *K8s) StopTunnel() error { return stopTunnel(k.tunnelContext) }

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
