package overview

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gorilla/mux"
	"github.com/heptio/developer-dash/internal/api"
	"github.com/heptio/developer-dash/internal/module"
	"github.com/heptio/developer-dash/internal/portforward"
	"github.com/heptio/developer-dash/pkg/plugin"

	"github.com/heptio/developer-dash/internal/cache"
	"github.com/heptio/developer-dash/internal/log"
	"github.com/heptio/developer-dash/internal/view/component"

	"github.com/heptio/developer-dash/internal/cluster"
	"github.com/heptio/developer-dash/internal/sugarloaf"
	"github.com/pkg/errors"
)

type Options struct {
	Client        cluster.ClientInterface
	Cache         cache.Cache
	Namespace     string
	Logger        log.Logger
	PluginManager *plugin.Manager
}

// ClusterOverview is an API for generating a cluster overview.
type ClusterOverview struct {
	client         cluster.ClientInterface
	logger         log.Logger
	cache          cache.Cache
	generator      *realGenerator
	portForwardSvc portforward.PortForwarder
	pluginManager  *plugin.Manager
}

// NewClusterOverview creates an instance of ClusterOverview.
func NewClusterOverview(ctx context.Context, options Options) (*ClusterOverview, error) {
	if options.Client == nil {
		return nil, errors.New("nil cluster client")
	}

	if options.PluginManager == nil {
		return nil, errors.New("plugin manager is nil")
	}

	di, err := options.Client.DiscoveryClient()
	if err != nil {
		return nil, errors.Wrapf(err, "creating DiscoveryClient")
	}

	pm := newPathMatcher()
	for _, pf := range rootDescriber.PathFilters() {
		pm.Register(ctx, pf)
	}

	for _, pf := range eventsDescriber.PathFilters() {
		pm.Register(ctx, pf)
	}

	crdAddFunc := func(pm *pathMatcher, csd *crdSectionDescriber) objectHandler {
		return func(ctx context.Context, object *unstructured.Unstructured) {
			if object == nil {
				return
			}
			addCRD(ctx, object.GetName(), pm, csd)
		}
	}(pm, customResourcesDescriber)
	crdDeleteFunc := func(pm *pathMatcher, csd *crdSectionDescriber) objectHandler {
		return func(ctx context.Context, object *unstructured.Unstructured) {
			if object == nil {
				return
			}
			deleteCRD(ctx, object.GetName(), pm, csd)
		}
	}(pm, customResourcesDescriber)

	go watchCRDs(ctx, options.Cache, crdAddFunc, crdDeleteFunc)

	// Port Forwarding
	restClient, err := options.Client.RESTClient()
	if err != nil {
		return nil, errors.Wrap(err, "fetching RESTClient")
	}
	pfOpts := portforward.ServiceOptions{
		RESTClient: restClient,
		Config:     options.Client.RESTConfig(),
		Cache:      options.Cache,
		PortForwarder: &portforward.DefaultPortForwarder{
			IOStreams: portforward.IOStreams{
				In:     os.Stdin,
				Out:    os.Stdout,
				ErrOut: os.Stderr,
			},
		},
	}
	pfSvc := portforward.New(ctx, pfOpts, options.Logger)

	g, err := newGenerator(options.Cache, di, pm, options.Client, pfSvc)
	if err != nil {
		return nil, errors.Wrap(err, "create overview generator")
	}

	co := &ClusterOverview{
		client:         options.Client,
		logger:         options.Logger,
		cache:          options.Cache,
		generator:      g,
		portForwardSvc: pfSvc,
		pluginManager:  options.PluginManager,
	}
	return co, nil
}

// Name returns the name for this module.
func (co *ClusterOverview) Name() string {
	return "overview"
}

// ContentPath returns the content path for overview.
func (co *ClusterOverview) ContentPath() string {
	return fmt.Sprintf("/%s", co.Name())
}

// Navigation returns navigation entries for overview.
func (co *ClusterOverview) Navigation(ctx context.Context, namespace, root string) (*sugarloaf.Navigation, error) {
	nf := NewNavigationFactory(namespace, root, co.cache)
	return nf.Entries(ctx)
}

// SetNamespace sets the current namespace.
func (co *ClusterOverview) SetNamespace(namespace string) error {
	co.logger.With("namespace", namespace, "module", "overview").Debugf("setting namespace (noop)")
	return nil
}

// Start starts overview.
func (co *ClusterOverview) Start() error {
	return nil
}

// Stop stops overview.
func (co *ClusterOverview) Stop() {
	// NOOP
}

// Content serves content for overview.
func (co *ClusterOverview) Content(ctx context.Context, contentPath, prefix, namespace string, opts module.ContentOptions) (component.ContentResponse, error) {
	ctx = log.WithLoggerContext(ctx, co.logger)
	genOpts := GeneratorOptions{
		Selector:       opts.Selector,
		PortForwardSvc: co.portForwardSvc,
		PluginManager:  co.pluginManager,
	}
	return co.generator.Generate(ctx, contentPath, prefix, namespace, genOpts)
}

type logEntry struct {
	Timestamp time.Time `json:"timestamp,omitempty"`
	Message   string    `json:"message,omitempty"`
}

type logResponse struct {
	Entries []logEntry `json:"entries,omitempty"`
}

// Handlers are extra handlers for overview
func (co *ClusterOverview) Handlers(ctx context.Context) map[string]http.Handler {
	return map[string]http.Handler{
		"/logs/pod/{pod}/container/{container}": containerLogsHandler(co.client),
		"/port-forwards":                        co.portForwardsHandler(),
		"/port-forwards/{id}":                   co.portForwardHandler(),
	}
}

func (co *ClusterOverview) portForwardsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := co.portForwardSvc
		if svc == nil {
			co.logger.Errorf("port forward service is nil")
			http.Error(w, "portforward service is nil", http.StatusInternalServerError)
			return
		}

		ctx := log.WithLoggerContext(r.Context(), co.logger)

		defer r.Body.Close()

		switch r.Method {
		case http.MethodPost:
			err := createPortforward(ctx, r.Body, co.portForwardSvc, w)
			handlePortforwardError(w, err, co.logger)
		default:
			api.RespondWithError(
				w,
				http.StatusNotFound,
				fmt.Sprintf("unhandled HTTP method %s", r.Method),
				co.logger,
			)
		}
	}
}

func (co *ClusterOverview) portForwardHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := co.portForwardSvc
		if svc == nil {
			co.logger.Errorf("port forward service is nil")
			http.Error(w, "portforward service is nil", http.StatusInternalServerError)
			return
		}

		vars := mux.Vars(r)
		id := vars["id"]

		ctx := log.WithLoggerContext(r.Context(), co.logger)

		switch r.Method {
		case http.MethodDelete:
			err := deletePortForward(ctx, id, co.portForwardSvc, w)
			handlePortforwardError(w, err, co.logger)
		default:
			api.RespondWithError(
				w,
				http.StatusNotFound,
				fmt.Sprintf("unhandled HTTP method %s", r.Method),
				co.logger,
			)
		}
	}
}
