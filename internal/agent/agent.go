// Package agent watches a cluster with the rules package and sends each new
// or resolved problem to paired phones through the relay.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/template"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gwinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	"github.com/RazaLabs/kubctl-agent/internal/pairing"
	"github.com/RazaLabs/kubctl-agent/internal/rules"
)

// Version is reported to the app; set at build time with -ldflags.
var Version = "0.1.0"

const (
	secretName = "kubctl-agent"
	configName = "kubctl-agent-config"
	stateName  = "kubctl-agent-state"
)

// Config comes from the environment (see FromEnv).
type Config struct {
	Rest        *rest.Config
	Namespace   string
	RelayURL    string
	ClusterName string
	RelayCA     string
	Addr        string
	Timing      rules.Timing
	// Tick is how often problems are re-checked without changes (10s).
	Tick time.Duration
}

// FromEnv reads the configuration. In a pod it uses the service account;
// elsewhere (development) KUBECONFIG, optionally with KUBE_CONTEXT.
//
//	RELAY_URL     where to send alerts
//	CLUSTER_NAME  the name shown in notifications
//	RELAY_CA      PEM file of extra CAs to trust for the relay
//	PORT          HTTP port for /healthz and /alertmanager (8080)
//	POD_NAMESPACE where the agent's Secret and ConfigMaps live (kubctl-system)
func FromEnv() (Config, error) {
	var rc *rest.Config
	var err error
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kc},
			&clientcmd.ConfigOverrides{CurrentContext: os.Getenv("KUBE_CONTEXT")},
		).ClientConfig()
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return Config{}, fmt.Errorf("connecting to the cluster: %w", err)
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "kubctl-system"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return Config{
		Rest: rc, Namespace: ns, RelayURL: pairing.TrimSlash(os.Getenv("RELAY_URL")),
		ClusterName: os.Getenv("CLUSTER_NAME"), RelayCA: os.Getenv("RELAY_CA"),
		Addr: ":" + port, Timing: rules.DefaultTiming,
	}, nil
}

// Agent is one running agent.
type Agent struct {
	cfg     Config
	kube    kubernetes.Interface
	gw      gwclient.Interface
	pairing pairing.Pairing
	relay   *RelayClient
	engine  *rules.Engine

	// Watched kinds: whether each has loaded, and its last watch error.
	informers map[rules.Kind]cache.SharedIndexInformer
	mu        sync.Mutex
	failed    map[rules.Kind]error

	listers snapshotFuncs
	trigger chan struct{}
	amIn    chan []AmAlert

	// Owned by the run loop.
	amActive  map[string]rules.Alert
	amLast    time.Time
	baselined bool
	details   bool
	lastState string
	written   time.Time
}

// New connects to the cluster and loads (or creates) the pairing keys.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	kube, err := kubernetes.NewForConfig(cfg.Rest)
	if err != nil {
		return nil, err
	}
	gw, err := gwclient.NewForConfig(cfg.Rest)
	if err != nil {
		return nil, err
	}
	return NewWithClients(ctx, cfg, kube, gw)
}

// NewWithClients is New with clients supplied (tests use fakes).
func NewWithClients(ctx context.Context, cfg Config, kube kubernetes.Interface, gw gwclient.Interface) (*Agent, error) {
	var err error
	a := &Agent{
		cfg: cfg, kube: kube, gw: gw, engine: rules.NewEngine(cfg.Timing),
		informers: map[rules.Kind]cache.SharedIndexInformer{}, failed: map[rules.Kind]error{},
		trigger: make(chan struct{}, 1), amIn: make(chan []AmAlert, 16),
		amActive: map[string]rules.Alert{}, details: true,
	}
	if a.pairing, err = a.ensurePairing(ctx); err != nil {
		return nil, fmt.Errorf("pairing keys: %w", err)
	}
	if a.relay, err = NewRelayClient(a.pairing, cfg.RelayCA); err != nil {
		return nil, err
	}
	a.baselined = a.restore(ctx)
	return a, nil
}

// ------------------------------------------------------------ pairing

// ensurePairing reads the Secret made by the app at install, or creates it
// on first start for hand installs. The deployment's settings win, so a
// reconfigured agent keeps phones paired to the right relay.
func (a *Agent) ensurePairing(ctx context.Context) (pairing.Pairing, error) {
	secrets := a.kube.CoreV1().Secrets(a.cfg.Namespace)
	s, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		if p, ok := pairing.FromSecretData(s.Data); ok {
			want := p
			if a.cfg.RelayURL != "" {
				want.RelayURL = a.cfg.RelayURL
			}
			if a.cfg.ClusterName != "" {
				want.Cluster = a.cfg.ClusterName
			}
			if want == p {
				return p, nil
			}
			s.Data = want.SecretData()
			_, err := secrets.Update(ctx, s, metav1.UpdateOptions{})
			return want, err
		}
	} else if !apierrors.IsNotFound(err) {
		return pairing.Pairing{}, err
	}
	name := a.cfg.ClusterName
	if name == "" {
		name = "cluster"
	}
	p := pairing.Generate(a.cfg.RelayURL, name)
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: a.cfg.Namespace, Labels: map[string]string{
			"app.kubernetes.io/name": "kubctl-agent", "app.kubernetes.io/managed-by": "kubctl",
		}},
		Type: corev1.SecretTypeOpaque,
		Data: p.SecretData(),
	}, metav1.CreateOptions{})
	if err == nil {
		slog.Info("created pairing keys", "secret", a.cfg.Namespace+"/"+secretName)
	}
	return p, err
}

func (a *Agent) readConfig(ctx context.Context) {
	cm, err := a.kube.CoreV1().ConfigMaps(a.cfg.Namespace).Get(ctx, configName, metav1.GetOptions{})
	a.details = err != nil || cm.Data["details"] != "false"
}

// ------------------------------------------------------------ state

// restore loads alerts from the last run; false on the very first start.
func (a *Agent) restore(ctx context.Context) bool {
	cm, err := a.kube.CoreV1().ConfigMaps(a.cfg.Namespace).Get(ctx, stateName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Warn("reading state", "err", err)
		}
		return false
	}
	raw, ok := cm.Data["alerts"]
	if !ok {
		return false
	}
	var alerts []rules.Alert
	if err := json.Unmarshal([]byte(raw), &alerts); err != nil {
		return false
	}
	var own []rules.Alert
	for _, al := range alerts {
		if al.Source == rules.FromAlertmanager {
			a.amActive[al.ID] = al
		} else {
			own = append(own, al)
		}
	}
	a.engine.Restore(own)
	return true
}

func (a *Agent) active() []rules.Alert {
	out := a.engine.Active()
	for _, al := range a.amActive {
		out = append(out, al)
	}
	return out
}

// writeState publishes current problems and a heartbeat for the app, when
// something changed and at least once a minute.
func (a *Agent) writeState(ctx context.Context, force bool) {
	delivered, relayErr, queued := a.relay.Status()
	alerts, _ := json.Marshal(a.active())
	relay := map[string]any{"url": a.pairing.RelayURL, "lastDelivered": nil, "error": nil, "queued": queued}
	if !delivered.IsZero() {
		relay["lastDelivered"] = delivered.UTC().Format(time.RFC3339Nano)
	}
	if relayErr != "" {
		relay["error"] = relayErr
	}
	relayJSON, _ := json.Marshal(relay)
	am := ""
	if !a.amLast.IsZero() {
		am = a.amLast.UTC().Format(time.RFC3339Nano)
	}
	data := map[string]string{
		"version": Version, "cluster": a.pairing.Cluster, "alerts": string(alerts),
		"relay": string(relayJSON), "alertmanager": am,
	}
	key, _ := json.Marshal(data)
	if !force && string(key) == a.lastState && time.Since(a.written) < time.Minute {
		return
	}
	data["heartbeat"] = time.Now().UTC().Format(time.RFC3339Nano)
	cms := a.kube.CoreV1().ConfigMaps(a.cfg.Namespace)
	patch, _ := json.Marshal(map[string]any{"data": data})
	_, err := cms.Patch(ctx, stateName, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: a.cfg.Namespace},
			Data:       data,
		}, metav1.CreateOptions{})
	}
	if err != nil {
		slog.Warn("writing state", "err", err)
		return
	}
	a.lastState, a.written = string(key), time.Now()
}

// ------------------------------------------------------------ watching

type snapshotFuncs []func(*rules.Objects)

func (a *Agent) schedule() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// watch starts an informer for kind, noting watch errors (forbidden, not
// served) so a kind that can't load doesn't hold up the first evaluation.
func (a *Agent) watch(kind rules.Kind, inf cache.SharedIndexInformer) {
	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		a.mu.Lock()
		a.failed[kind] = err
		a.mu.Unlock()
		slog.Warn("watch failed", "kind", kind, "err", err)
		a.schedule()
	})
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { a.schedule() },
		UpdateFunc: func(any, any) { a.schedule() },
		DeleteFunc: func(any) { a.schedule() },
	})
	a.informers[kind] = inf
}

func (a *Agent) startInformers(ctx context.Context) {
	f := informers.NewSharedInformerFactory(a.kube, 0)
	pods, nodes, pvcs := f.Core().V1().Pods(), f.Core().V1().Nodes(), f.Core().V1().PersistentVolumeClaims()
	deps, sts, dss, jobs := f.Apps().V1().Deployments(), f.Apps().V1().StatefulSets(), f.Apps().V1().DaemonSets(), f.Batch().V1().Jobs()
	a.watch(rules.Pods, pods.Informer())
	a.watch(rules.Nodes, nodes.Informer())
	a.watch(rules.PVCs, pvcs.Informer())
	a.watch(rules.Deployments, deps.Informer())
	a.watch(rules.StatefulSets, sts.Informer())
	a.watch(rules.DaemonSets, dss.Informer())
	a.watch(rules.Jobs, jobs.Informer())
	all := labels.Everything()
	a.listers = snapshotFuncs{
		func(o *rules.Objects) { o.Pods, _ = pods.Lister().List(all) },
		func(o *rules.Objects) { o.Nodes, _ = nodes.Lister().List(all) },
		func(o *rules.Objects) { o.PVCs, _ = pvcs.Lister().List(all) },
		func(o *rules.Objects) { o.Deployments, _ = deps.Lister().List(all) },
		func(o *rules.Objects) { o.StatefulSets, _ = sts.Lister().List(all) },
		func(o *rules.Objects) { o.DaemonSets, _ = dss.Lister().List(all) },
		func(o *rules.Objects) { o.Jobs, _ = jobs.Lister().List(all) },
	}
	f.Start(ctx.Done())

	// The Gateway API is optional: watch it only where it's installed.
	if served(a.kube, "gateway.networking.k8s.io/v1", "gateways", "httproutes") {
		g := gwinformers.NewSharedInformerFactory(a.gw, 0)
		gws, routes := g.Gateway().V1().Gateways(), g.Gateway().V1().HTTPRoutes()
		a.watch(rules.Gateways, gws.Informer())
		a.watch(rules.HTTPRoutes, routes.Informer())
		a.listers = append(a.listers,
			func(o *rules.Objects) { o.Gateways, _ = gws.Lister().List(all) },
			func(o *rules.Objects) { o.HTTPRoutes, _ = routes.Lister().List(all) },
		)
		g.Start(ctx.Done())
	}
}

func served(kube kubernetes.Interface, groupVersion string, resources ...string) bool {
	list, err := kube.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return false
	}
	have := map[string]bool{}
	for _, r := range list.APIResources {
		have[r.Name] = true
	}
	for _, r := range resources {
		if !have[r] {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------ evaluation

func (a *Agent) evaluate(ctx context.Context) {
	ready := map[rules.Kind]bool{}
	a.mu.Lock()
	for kind, inf := range a.informers {
		switch {
		case inf.HasSynced():
			ready[kind] = true
		case a.failed[kind] == nil && !a.baselined:
			// Wait until every kind has loaded or failed, so the first look is complete.
			a.mu.Unlock()
			return
		}
	}
	a.mu.Unlock()

	var objs rules.Objects
	for _, list := range a.listers {
		list(&objs)
	}
	rows := rules.Rows(objs)
	var inScope []rules.Row
	for _, r := range rows {
		if ready[r.Kind] {
			inScope = append(inScope, r)
		}
	}
	first := !a.baselined
	ch := a.engine.Update(inScope, time.Now(), ready, first)
	if first {
		a.baselined = true
		a.hello()
	}
	for _, al := range ch.Fired {
		a.push(al, false)
	}
	for _, al := range ch.Resolved {
		a.push(al, true)
	}
	a.writeState(ctx, !ch.Empty() || first)
}

func (a *Agent) push(al rules.Alert, resolved bool) {
	m, err := pushFor(a.pairing, al, resolved, a.details)
	if err != nil {
		slog.Error("sealing alert", "err", err)
		return
	}
	slog.Info("alert", "id", al.ID, "resolved", resolved, "title", al.Title)
	a.relay.Send(m)
}

// hello is one notification after install, so phones know it works.
func (a *Agent) hello() {
	now := a.active()
	summary := "Everything looks healthy right now."
	if n := len(now); n > 0 {
		noun := "problems"
		if n == 1 {
			noun = "problem"
		}
		summary = fmt.Sprintf("%d %s right now: %s", n, noun, now[0].Title)
		if n > 1 {
			summary += "; " + now[1].Title
		}
		if n > 2 {
			summary += "…"
		}
	}
	body := "kubctl is now watching this cluster."
	if a.details {
		body += " " + summary
	}
	sealed, err := pairing.Seal(a.pairing.SealKey, map[string]any{"cluster": a.pairing.ClusterID(), "state": "hello", "alerts": len(now)})
	if err != nil {
		return
	}
	a.relay.Send(PushMessage{ID: "kubctl:hello", State: "firing", Title: a.pairing.Cluster, Body: body, Sealed: sealed})
}

func (a *Agent) alertmanager(ctx context.Context, alerts []AmAlert) {
	a.amLast = time.Now()
	for _, am := range alerts {
		_, known := a.amActive[am.Alert.ID]
		switch {
		case am.Firing && !known:
			a.amActive[am.Alert.ID] = am.Alert
			a.push(am.Alert, false)
		case !am.Firing && known:
			delete(a.amActive, am.Alert.ID)
			a.push(am.Alert, true)
		}
	}
	a.writeState(ctx, true)
}

// ------------------------------------------------------------ running

// Handler serves /healthz and Alertmanager's webhook at /alertmanager.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /alertmanager", func(w http.ResponseWriter, r *http.Request) {
		var data template.Data
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&data); err != nil {
			http.Error(w, "body must be an Alertmanager webhook", http.StatusBadRequest)
			return
		}
		a.amIn <- FromAlertmanager(data)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// Run watches and alerts until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.readConfig(ctx)
	a.startInformers(ctx)
	go a.relay.Run(ctx)

	srv := &http.Server{Addr: a.cfg.Addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	slog.Info("kubctl agent watching", "version", Version, "cluster", a.pairing.Cluster, "relay", a.pairing.RelayURL)

	every := a.cfg.Tick
	if every == 0 {
		every = 10 * time.Second
	}
	tick := time.NewTicker(every)
	config := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	defer config.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		case err := <-errs:
			return err
		case <-a.trigger:
			if debounce == nil {
				debounce = time.After(2 * time.Second)
			}
		case <-debounce:
			debounce = nil
			a.evaluate(ctx)
		case <-tick.C:
			a.evaluate(ctx)
		case <-config.C:
			a.readConfig(ctx)
		case alerts := <-a.amIn:
			a.alertmanager(ctx, alerts)
		}
	}
}
