package gateway

import (
	"net/http"
	"net/url"

	"github.com/codegangsta/negroni"
	"github.com/gorilla/mux"
	"github.com/meatballhat/negroni-logrus"
	"github.com/pkg/errors"
	"github.com/vulcand/oxy/cbreaker"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/roundrobin"
	"github.com/vulcand/oxy/stream"

	log "github.com/Sirupsen/logrus"
)

// DefaultServer is the default gateway server implementation
type DefaultServer struct {
	server        *http.Server
	router        *mux.Router
	proxyRoutes   map[string]*roundrobin.RoundRobin
	middleware    []negroni.Handler
	configuration *ServerConfiguration
}

// NewDefaultServer creates a new DefaultServer. If the router parameter is nil
// the method will create a new router. If the middleware parameter is nil the
// method will use a request id, logger and a recovery middleware.
func NewDefaultServer(config *ServerConfiguration) *DefaultServer {
	router := config.Router
	if router == nil {
		router = mux.NewRouter()
	}

	middleware := config.Middleware
	if len(middleware) <= 0 {
		middleware = append(middleware, NewRequestID())
		middleware = append(middleware, negronilogrus.NewMiddleware())
		middleware = append(middleware, negroni.NewRecovery())
	}

	addr := config.Address
	if addr == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	log.Debugln("creating new gateway server for", addr)
	return &DefaultServer{
		server:        server,
		router:        router,
		proxyRoutes:   map[string]*roundrobin.RoundRobin{},
		middleware:    middleware,
		configuration: config,
	}
}

func (ds *DefaultServer) updateProxyRoute(proxyRoute *ProxyRoute, lb *roundrobin.RoundRobin) error {
	log.Debugln("update proxy route for service", proxyRoute.Name)
	servers := lb.Servers()
	for _, url := range proxyRoute.Backends {
		if !ContainsURL(servers, url) {
			log.Infoln("register new backend", url)
			lb.UpsertServer(url)
		}
	}
	for _, url := range servers {
		if !ContainsURL(proxyRoute.Backends, url) {
			log.Infoln("unregister backend", url)
			lb.RemoveServer(url)
		}
	}
	return nil
}

func (ds *DefaultServer) addProxyRoute(proxyRoute *ProxyRoute) (*roundrobin.RoundRobin, error) {
	log.Debugln("add proxy route for service", proxyRoute.Name)
	fwd, err := forward.New()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create forward")
	}

	lb, err := roundrobin.New(fwd)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create roundrobin load balancer")
	}

	stream, err := stream.New(lb, stream.Retry(`IsNetworkError() && Attempts() < 2`))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create stream")
	}

	circuitBreaker, err := cbreaker.New(stream, "NetworkErrorRatio() > 0.5")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create circuit breaker")
	}

	for _, url := range proxyRoute.Backends {
		log.Infoln("register new backend for service", proxyRoute.Name, url)
		err = lb.UpsertServer(url)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create upsert server for %s", url.String())
		}
	}

	// configure middleware for proxy backend
	middleware := ds.createMiddleware(lb)
	middleware.UseHandler(circuitBreaker)
	route, err := proxyRoute.Create(ds.router)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to configure route for service %s", proxyRoute.Name)
	}

	// configure route
	route.Handler(middleware)

	return lb, nil
}

func (ds *DefaultServer) createMiddleware(lb *roundrobin.RoundRobin) *negroni.Negroni {
	// copy midlewares and append 502 handler
	length := len(ds.middleware)
	middlewares := make([]negroni.Handler, length+1)
	copy(middlewares, ds.middleware)
	middlewares[length] = &BadGateway{lb}

	// configure middleware for proxy backend
	return negroni.New(middlewares...)
}

// ConfigureProxyRoutes configures proxy routes. The method will configure a
// roundrobin load balancer for each proxy route.
func (ds *DefaultServer) ConfigureProxyRoutes(routes []*ProxyRoute) error {
	log.Debugln("configure proxy routes")

	// handle new and update
	for _, route := range routes {
		lb := ds.proxyRoutes[route.Name]
		if lb != nil {
			err := ds.updateProxyRoute(route, lb)
			if err != nil {
				return errors.Wrapf(err, "failed to update proxy route for %s", route.Name)
			}
		} else {
			lb, err := ds.addProxyRoute(route)
			if err != nil {
				return errors.Wrapf(err, "failed to add proxy route for %s", route.Name)
			}
			ds.proxyRoutes[route.Name] = lb
		}
	}

	// handle remove
	for name, lb := range ds.proxyRoutes {
		if !ContainsRoute(routes, name) {
			// Remove route completly ?
			route := ProxyRoute{Name: name, Backends: []*url.URL{}}
			ds.updateProxyRoute(&route, lb)
		}
	}

	return nil
}

// GetProxyRoutes returns a slice of current configured proxy routes.
func (ds *DefaultServer) GetProxyRoutes() []*ProxyRoute {
	routes := []*ProxyRoute{}
	for name, lb := range ds.proxyRoutes {
		backends := []*url.URL{}
		for _, url := range lb.Servers() {
			backends = append(backends, url)
		}
		routes = append(routes, &ProxyRoute{Name: name, Backends: backends})
	}
	return routes
}

// Start will start the default gateway server. After the server is started the
// ConfigureProxyRoutes can be used to reconfigure the gateway.
func (ds *DefaultServer) Start() error {
	if ds.configuration.CertFile != "" && ds.configuration.KeyFile != "" {
		log.Infoln("starting https gateway server")
		return ds.server.ListenAndServeTLS(ds.configuration.CertFile, ds.configuration.KeyFile)
	}
	log.Infoln("starting http gateway server")
	return ds.server.ListenAndServe()
}
