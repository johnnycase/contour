// Copyright Project Contour Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dag

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	projcontour "github.com/projectcontour/contour/apis/projectcontour/v1"
	"github.com/projectcontour/contour/internal/annotation"
	"github.com/projectcontour/contour/internal/k8s"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// HTTPProxyProcessor translates HTTPProxies into DAG
// objects and adds them to the DAG builder.
type HTTPProxyProcessor struct {
	builder  *Builder
	orphaned map[types.NamespacedName]bool

	// DisablePermitInsecure disables the use of the
	// permitInsecure field in HTTPProxy.
	DisablePermitInsecure bool

	// FallbackCertificate is the optional identifier of the
	// TLS secret to use by default when SNI is not set on a
	// request.
	FallbackCertificate *types.NamespacedName
}

// Run translates HTTPProxies into DAG objects and
// adds them to the DAG builder.
func (p *HTTPProxyProcessor) Run(builder *Builder) {
	p.builder = builder
	p.orphaned = make(map[types.NamespacedName]bool, len(p.orphaned))

	// reset the processor when we're done
	defer func() {
		p.builder = nil
		p.orphaned = nil
	}()

	p.computeHTTPProxies()

	for meta := range p.orphaned {
		proxy, ok := p.builder.Source.httpproxies[meta]
		if ok {
			sw, commit := p.builder.WithObject(proxy)
			sw.WithValue("status", k8s.StatusOrphaned).
				WithValue("description", "this HTTPProxy is not part of a delegation chain from a root HTTPProxy")
			commit()
		}
	}
}

func (p *HTTPProxyProcessor) computeHTTPProxies() {
	for _, proxy := range p.validHTTPProxies() {
		p.computeHTTPProxy(proxy)
	}
}

func (p *HTTPProxyProcessor) computeHTTPProxy(proxy *projcontour.HTTPProxy) {
	sw, commit := p.builder.WithObject(proxy)
	defer commit()

	if proxy.Spec.VirtualHost == nil {
		// mark HTTPProxy as orphaned.
		p.setOrphaned(proxy)
		return
	}

	// ensure root httpproxy lives in allowed namespace
	if !p.rootAllowed(proxy.Namespace) {
		sw.SetInvalid("root HTTPProxy cannot be defined in this namespace")
		return
	}

	host := proxy.Spec.VirtualHost.Fqdn
	if isBlank(host) {
		sw.SetInvalid("Spec.VirtualHost.Fqdn must be specified")
		return
	}
	sw = sw.WithValue("vhost", host)
	if strings.Contains(host, "*") {
		sw.SetInvalid("Spec.VirtualHost.Fqdn %q cannot use wildcards", host)
		return
	}

	if len(proxy.Spec.Routes) == 0 && len(proxy.Spec.Includes) == 0 && proxy.Spec.TCPProxy == nil {
		sw.SetInvalid("HTTPProxy.Spec must have at least one Route, Include, or a TCPProxy")
		return
	}

	var tlsEnabled bool
	if tls := proxy.Spec.VirtualHost.TLS; tls != nil {
		if !isBlank(tls.SecretName) && tls.Passthrough {
			sw.SetInvalid("Spec.VirtualHost.TLS: both Passthrough and SecretName were specified")
			return
		}
		if isBlank(tls.SecretName) && !tls.Passthrough {
			sw.SetInvalid("Spec.VirtualHost.TLS: neither Passthrough nor SecretName were specified")
			return
		}
		tlsEnabled = true

		// Attach secrets to TLS enabled vhosts.
		if !tls.Passthrough {
			secretName := k8s.NamespacedNameFrom(tls.SecretName, k8s.DefaultNamespace(proxy.Namespace))
			sec, err := p.builder.Source.LookupSecret(secretName, validSecret)
			if err != nil {
				sw.SetInvalid("Spec.VirtualHost.TLS Secret %q is invalid: %s", tls.SecretName, err)
				return
			}

			if !p.builder.Source.DelegationPermitted(secretName, proxy.Namespace) {
				sw.SetInvalid("Spec.VirtualHost.TLS Secret %q certificate delegation not permitted", tls.SecretName)
				return
			}

			svhost := p.builder.lookupSecureVirtualHost(host)
			svhost.Secret = sec
			svhost.MinTLSVersion = annotation.MinTLSVersion(tls.MinimumProtocolVersion)

			// Check if FallbackCertificate && ClientValidation are both enabled in the same vhost
			if tls.EnableFallbackCertificate && tls.ClientValidation != nil {
				sw.SetInvalid("Spec.Virtualhost.TLS fallback & client validation are incompatible together")
				return
			}

			// If FallbackCertificate is enabled, but no cert passed, set error
			if tls.EnableFallbackCertificate {
				if p.FallbackCertificate == nil {
					sw.SetInvalid("Spec.Virtualhost.TLS enabled fallback but the fallback Certificate Secret is not configured in Contour configuration file")
					return
				}

				sec, err = p.builder.Source.LookupSecret(*p.FallbackCertificate, validSecret)
				if err != nil {
					sw.SetInvalid("Spec.Virtualhost.TLS Secret %q fallback certificate is invalid: %s", p.FallbackCertificate, err)
					return
				}

				if !p.builder.Source.DelegationPermitted(*p.FallbackCertificate, proxy.Namespace) {
					sw.SetInvalid("Spec.VirtualHost.TLS fallback Secret %q is not configured for certificate delegation", p.FallbackCertificate)
					return
				}

				svhost.FallbackCertificate = sec
			}

			// Fill in DownstreamValidation when external client validation is enabled.
			if tls.ClientValidation != nil {
				dv, err := p.lookupDownstreamValidation(tls.ClientValidation, proxy.Namespace)
				if err != nil {
					sw.SetInvalid("Spec.VirtualHost.TLS client validation is invalid: %s", err)
					return
				}
				svhost.DownstreamValidation = dv
			}
		} else if tls.ClientValidation != nil {
			sw.SetInvalid("Spec.VirtualHost.TLS passthrough cannot be combined with tls.clientValidation")
			return
		}
	}

	if proxy.Spec.TCPProxy != nil {
		if !tlsEnabled {
			sw.SetInvalid("Spec.TCPProxy requires that either Spec.TLS.Passthrough or Spec.TLS.SecretName be set")
			return
		}
		if !p.processHTTPProxyTCPProxy(sw, proxy, nil, host) {
			return
		}
	}

	routes := p.computeRoutes(sw, proxy, nil, nil, tlsEnabled)
	insecure := p.builder.lookupVirtualHost(host)
	addRoutes(insecure, routes)

	// if TLS is enabled for this virtual host and there is no tcp proxy defined,
	// then add routes to the secure virtualhost definition.
	if tlsEnabled && proxy.Spec.TCPProxy == nil {
		secure := p.builder.lookupSecureVirtualHost(host)
		addRoutes(secure, routes)
	}
}

type vhost interface {
	addRoute(*Route)
}

// addRoutes adds all routes to the vhost supplied.
func addRoutes(vhost vhost, routes []*Route) {
	for _, route := range routes {
		vhost.addRoute(route)
	}
}

func (p *HTTPProxyProcessor) computeRoutes(sw *ObjectStatusWriter, proxy *projcontour.HTTPProxy, conditions []projcontour.MatchCondition, visited []*projcontour.HTTPProxy, enforceTLS bool) []*Route {
	for _, v := range visited {
		// ensure we are not following an edge that produces a cycle
		var path []string
		for _, vir := range visited {
			path = append(path, fmt.Sprintf("%s/%s", vir.Namespace, vir.Name))
		}
		if v.Name == proxy.Name && v.Namespace == proxy.Namespace {
			path = append(path, fmt.Sprintf("%s/%s", proxy.Namespace, proxy.Name))
			sw.SetInvalid("include creates a delegation cycle: %s", strings.Join(path, " -> "))
			return nil
		}
	}

	visited = append(visited, proxy)
	var routes []*Route

	// Check for duplicate conditions on the includes
	if includeMatchConditionsIdentical(proxy.Spec.Includes) {
		sw.SetInvalid("duplicate conditions defined on an include")
		return nil
	}

	// Loop over and process all includes
	for _, include := range proxy.Spec.Includes {
		namespace := include.Namespace
		if namespace == "" {
			namespace = proxy.Namespace
		}

		delegate, ok := p.builder.Source.httpproxies[types.NamespacedName{Name: include.Name, Namespace: namespace}]
		if !ok {
			sw.SetInvalid("include %s/%s not found", namespace, include.Name)
			return nil
		}
		if delegate.Spec.VirtualHost != nil {
			sw.SetInvalid("root httpproxy cannot delegate to another root httpproxy")
			return nil
		}

		if err := pathMatchConditionsValid(include.Conditions); err != nil {
			sw.SetInvalid("include: %s", err)
			return nil
		}

		sw, commit := p.builder.WithObject(delegate)
		routes = append(routes, p.computeRoutes(sw, delegate, append(conditions, include.Conditions...), visited, enforceTLS)...)
		commit()

		// dest is not an orphaned httpproxy, as there is an httpproxy that points to it
		delete(p.orphaned, types.NamespacedName{Name: delegate.Name, Namespace: delegate.Namespace})
	}

	for _, route := range proxy.Spec.Routes {
		if err := pathMatchConditionsValid(route.Conditions); err != nil {
			sw.SetInvalid("route: %s", err)
			return nil
		}

		conds := append(conditions, route.Conditions...)

		// Look for invalid header conditions on this route
		if err := headerMatchConditionsValid(conds); err != nil {
			sw.SetInvalid(err.Error())
			return nil
		}

		reqHP, err := headersPolicy(route.RequestHeadersPolicy, true /* allow Host */)
		if err != nil {
			sw.SetInvalid(err.Error())
			return nil
		}

		respHP, err := headersPolicy(route.ResponseHeadersPolicy, false /* disallow Host */)
		if err != nil {
			sw.SetInvalid(err.Error())
			return nil
		}

		if len(route.Services) < 1 {
			sw.SetInvalid("route.services must have at least one entry")
			return nil
		}

		r := &Route{
			PathMatchCondition:    mergePathMatchConditions(conds),
			HeaderMatchConditions: mergeHeaderMatchConditions(conds),
			Websocket:             route.EnableWebsockets,
			HTTPSUpgrade:          routeEnforceTLS(enforceTLS, route.PermitInsecure && !p.DisablePermitInsecure),
			TimeoutPolicy:         timeoutPolicy(route.TimeoutPolicy),
			RetryPolicy:           retryPolicy(route.RetryPolicy),
			RequestHeadersPolicy:  reqHP,
			ResponseHeadersPolicy: respHP,
		}

		if len(route.GetPrefixReplacements()) > 0 {
			if !r.HasPathPrefix() {
				sw.SetInvalid("cannot specify prefix replacements without a prefix condition")
				return nil
			}

			if err := prefixReplacementsAreValid(route.GetPrefixReplacements()); err != nil {
				sw.SetInvalid(err.Error())
				return nil
			}

			// Note that we are guaranteed to always have a prefix
			// condition. Even if the CRD user didn't specify a
			// prefix condition, mergePathConditions() guarantees
			// a prefix of '/'.
			routingPrefix := r.PathMatchCondition.(*PrefixMatchCondition).Prefix

			// First, try to apply an exact prefix match.
			for _, prefix := range route.GetPrefixReplacements() {
				if len(prefix.Prefix) > 0 && routingPrefix == prefix.Prefix {
					r.PrefixRewrite = prefix.Replacement
					break
				}
			}

			// If there wasn't a match, we can apply the default replacement.
			if len(r.PrefixRewrite) == 0 {
				for _, prefix := range route.GetPrefixReplacements() {
					if len(prefix.Prefix) == 0 {
						r.PrefixRewrite = prefix.Replacement
						break
					}
				}
			}

		}

		for _, service := range route.Services {
			if service.Port < 1 || service.Port > 65535 {
				sw.SetInvalid("service %q: port must be in the range 1-65535", service.Name)
				return nil
			}
			m := types.NamespacedName{Name: service.Name, Namespace: proxy.Namespace}
			s, err := p.builder.lookupService(m, intstr.FromInt(service.Port))
			if err != nil {
				sw.SetInvalid("Spec.Routes unresolved service reference: %s", err)
				return nil
			}

			// Determine the protocol to use to speak to this Cluster.
			protocol, err := getProtocol(service, s)
			if err != nil {
				sw.SetInvalid(err.Error())
				return nil
			}

			var uv *PeerValidationContext
			if protocol == "tls" || protocol == "h2" {
				// we can only validate TLS connections to services that talk TLS
				uv, err = p.lookupUpstreamValidation(service.UpstreamValidation, proxy.Namespace)
				if err != nil {
					sw.SetInvalid("Service [%s:%d] TLS upstream validation policy error: %s",
						service.Name, service.Port, err)
					return nil
				}
			}

			reqHP, err := headersPolicy(service.RequestHeadersPolicy, true /* allow Host */)
			if err != nil {
				sw.SetInvalid(err.Error())
				return nil
			}

			respHP, err := headersPolicy(service.ResponseHeadersPolicy, false /* disallow Host */)
			if err != nil {
				sw.SetInvalid(err.Error())
				return nil
			}

			c := &Cluster{
				Upstream:              s,
				LoadBalancerPolicy:    loadBalancerPolicy(route.LoadBalancerPolicy),
				Weight:                uint32(service.Weight),
				HTTPHealthCheckPolicy: httpHealthCheckPolicy(route.HealthCheckPolicy),
				UpstreamValidation:    uv,
				RequestHeadersPolicy:  reqHP,
				ResponseHeadersPolicy: respHP,
				Protocol:              protocol,
				SNI:                   determineSNI(r.RequestHeadersPolicy, reqHP, s),
			}
			if service.Mirror && r.MirrorPolicy != nil {
				sw.SetInvalid("only one service per route may be nominated as mirror")
				return nil
			}
			if service.Mirror {
				r.MirrorPolicy = &MirrorPolicy{
					Cluster: c,
				}
			} else {
				r.Clusters = append(r.Clusters, c)
			}
		}
		routes = append(routes, r)
	}

	routes = expandPrefixMatches(routes)

	sw.SetValid()
	return routes
}

// processHTTPProxyTCPProxy processes the spec.tcpproxy stanza in a HTTPProxy document
// following the chain of spec.tcpproxy.include references. It returns true if processing
// was successful, otherwise false if an error was encountered. The details of the error
// will be recorded on the status of the relevant HTTPProxy object,
func (p *HTTPProxyProcessor) processHTTPProxyTCPProxy(sw *ObjectStatusWriter, httpproxy *projcontour.HTTPProxy, visited []*projcontour.HTTPProxy, host string) bool {
	tcpproxy := httpproxy.Spec.TCPProxy
	if tcpproxy == nil {
		// nothing to do
		return true
	}

	visited = append(visited, httpproxy)

	// #2218 Allow support for both plural and singular "Include" for TCPProxy for the v1 API Spec
	// Prefer configurations for singular over the plural version
	tcpProxyInclude := tcpproxy.Include
	if tcpproxy.Include == nil {
		tcpProxyInclude = tcpproxy.IncludesDeprecated
	}

	if len(tcpproxy.Services) > 0 && tcpProxyInclude != nil {
		sw.SetInvalid("tcpproxy: cannot specify services and include in the same httpproxy")
		return false
	}

	if len(tcpproxy.Services) > 0 {
		var proxy TCPProxy
		for _, service := range httpproxy.Spec.TCPProxy.Services {
			m := types.NamespacedName{Name: service.Name, Namespace: httpproxy.Namespace}
			s, err := p.builder.lookupService(m, intstr.FromInt(service.Port))
			if err != nil {
				sw.SetInvalid("Spec.TCPProxy unresolved service reference: %s", err)
				return false
			}
			proxy.Clusters = append(proxy.Clusters, &Cluster{
				Upstream:             s,
				Protocol:             s.Protocol,
				LoadBalancerPolicy:   loadBalancerPolicy(tcpproxy.LoadBalancerPolicy),
				TCPHealthCheckPolicy: tcpHealthCheckPolicy(tcpproxy.HealthCheckPolicy),
			})
		}
		p.builder.lookupSecureVirtualHost(host).TCPProxy = &proxy
		return true
	}

	if tcpProxyInclude == nil {
		// We don't allow an empty TCPProxy object.
		sw.SetInvalid("tcpproxy: either services or inclusion must be specified")
		return false
	}

	namespace := tcpProxyInclude.Namespace
	if namespace == "" {
		// we are delegating to another HTTPProxy in the same namespace
		namespace = httpproxy.Namespace
	}

	m := types.NamespacedName{Name: tcpProxyInclude.Name, Namespace: namespace}
	dest, ok := p.builder.Source.httpproxies[m]
	if !ok {
		sw.SetInvalid("tcpproxy: include %s/%s not found", m.Namespace, m.Name)
		return false
	}

	if dest.Spec.VirtualHost != nil {
		sw.SetInvalid("root httpproxy cannot delegate to another root httpproxy")
		return false
	}

	// dest is no longer an orphan
	delete(p.orphaned, k8s.NamespacedNameOf(dest))

	// ensure we are not following an edge that produces a cycle
	var path []string
	for _, hp := range visited {
		path = append(path, fmt.Sprintf("%s/%s", hp.Namespace, hp.Name))
	}
	for _, hp := range visited {
		if dest.Name == hp.Name && dest.Namespace == hp.Namespace {
			path = append(path, fmt.Sprintf("%s/%s", dest.Namespace, dest.Name))
			sw.SetInvalid("tcpproxy include creates a cycle: %s", strings.Join(path, " -> "))
			return false
		}
	}

	// follow the link and process the target tcpproxy
	sw, commit := sw.WithObject(dest)
	defer commit()
	ok = p.processHTTPProxyTCPProxy(sw, dest, visited, host)
	if ok {
		sw.SetValid()
	}
	return ok
}

// validHTTPProxies returns a slice of *projcontour.HTTPProxy objects.
// invalid HTTPProxy objects are excluded from the slice and their status
// updated accordingly.
func (p *HTTPProxyProcessor) validHTTPProxies() []*projcontour.HTTPProxy {
	// ensure that a given fqdn is only referenced in a single HTTPProxy resource
	var valid []*projcontour.HTTPProxy
	fqdnHTTPProxies := make(map[string][]*projcontour.HTTPProxy)
	for _, proxy := range p.builder.Source.httpproxies {
		if proxy.Spec.VirtualHost == nil {
			valid = append(valid, proxy)
			continue
		}
		fqdnHTTPProxies[proxy.Spec.VirtualHost.Fqdn] = append(fqdnHTTPProxies[proxy.Spec.VirtualHost.Fqdn], proxy)
	}

	for fqdn, proxies := range fqdnHTTPProxies {
		switch len(proxies) {
		case 1:
			valid = append(valid, proxies[0])
		default:
			// multiple irs use the same fqdn. mark them as invalid.
			var conflicting []string
			for _, proxy := range proxies {
				conflicting = append(conflicting, proxy.Namespace+"/"+proxy.Name)
			}
			sort.Strings(conflicting) // sort for test stability
			msg := fmt.Sprintf("fqdn %q is used in multiple HTTPProxies: %s", fqdn, strings.Join(conflicting, ", "))
			for _, proxy := range proxies {
				sw, commit := p.builder.WithObject(proxy)
				sw.WithValue("vhost", fqdn).SetInvalid(msg)
				commit()
			}
		}
	}
	return valid
}

// rootAllowed returns true if the HTTPProxy lives in a permitted root namespace.
func (p *HTTPProxyProcessor) rootAllowed(namespace string) bool {
	if len(p.builder.Source.RootNamespaces) == 0 {
		return true
	}
	for _, ns := range p.builder.Source.RootNamespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

func (p *HTTPProxyProcessor) lookupUpstreamValidation(uv *projcontour.UpstreamValidation, namespace string) (*PeerValidationContext, error) {
	if uv == nil {
		// no upstream validation requested, nothing to do
		return nil, nil
	}

	secretName := types.NamespacedName{Name: uv.CACertificate, Namespace: namespace}
	cacert, err := p.builder.Source.LookupSecret(secretName, validCA)
	if err != nil {
		// UpstreamValidation is requested, but cert is missing or not configured
		return nil, fmt.Errorf("invalid CA Secret %q: %s", secretName, err)
	}

	if uv.SubjectName == "" {
		// UpstreamValidation is requested, but SAN is not provided
		return nil, errors.New("missing subject alternative name")
	}

	return &PeerValidationContext{
		CACertificate: cacert,
		SubjectName:   uv.SubjectName,
	}, nil
}

func (p *HTTPProxyProcessor) lookupDownstreamValidation(vc *projcontour.DownstreamValidation, namespace string) (*PeerValidationContext, error) {
	secretName := types.NamespacedName{Name: vc.CACertificate, Namespace: namespace}
	cacert, err := p.builder.Source.LookupSecret(secretName, validCA)
	if err != nil {
		// PeerValidationContext is requested, but cert is missing or not configured.
		return nil, fmt.Errorf("invalid CA Secret %q: %s", secretName, err)
	}

	return &PeerValidationContext{
		CACertificate: cacert,
	}, nil
}

func validCA(s *v1.Secret) error {
	if len(s.Data[CACertificateKey]) == 0 {
		return fmt.Errorf("empty %q key", CACertificateKey)
	}

	return nil
}

// setOrphaned records an HTTPProxy resource as orphaned.
func (p *HTTPProxyProcessor) setOrphaned(obj k8s.Object) {
	m := types.NamespacedName{
		Name:      obj.GetObjectMeta().GetName(),
		Namespace: obj.GetObjectMeta().GetNamespace(),
	}
	p.orphaned[m] = true
}

// expandPrefixMatches adds new Routes to account for the difference
// between prefix replacement when matching on '/foo' and '/foo/'.
//
// The table below shows the behavior of Envoy prefix rewrite. If we
// match on only `/foo` or `/foo/`, then the unwanted rewrites marked
// with X can result. This means that we need to generate separate
// prefix matches (and replacements) for these cases.
//
// | Matching Prefix | Replacement | Client Path | Rewritten Path |
// |-----------------|-------------|-------------|----------------|
// | `/foo`          | `/bar`      | `/foosball` |   `/barsball`  |
// | `/foo`          | `/`         | `/foo/v1`   | X `//v1`       |
// | `/foo/`         | `/bar`      | `/foo/type` | X `/bartype`   |
// | `/foo`          | `/bar/`     | `/foosball` | X `/bar/sball` |
// | `/foo/`         | `/bar/`     | `/foo/type` |   `/bar/type`  |
func expandPrefixMatches(routes []*Route) []*Route {
	prefixedRoutes := map[string][]*Route{}

	expandedRoutes := []*Route{}

	// First, we group the Routes by their slash-consistent prefix match condition.
	for _, r := range routes {
		// If there is no path prefix, we won't do any expansion, so skip it.
		if !r.HasPathPrefix() {
			expandedRoutes = append(expandedRoutes, r)
		}

		routingPrefix := r.PathMatchCondition.(*PrefixMatchCondition).Prefix

		if routingPrefix != "/" {
			routingPrefix = strings.TrimRight(routingPrefix, "/")
		}

		prefixedRoutes[routingPrefix] = append(prefixedRoutes[routingPrefix], r)
	}

	for prefix, routes := range prefixedRoutes {
		// Propagate the Routes into the expanded set. Since
		// we have a slice of pointers, we can propagate here
		// prior to any Route modifications.
		expandedRoutes = append(expandedRoutes, routes...)

		switch len(routes) {
		case 1:
			// Don't modify if we are not doing a replacement.
			if len(routes[0].PrefixRewrite) == 0 {
				continue
			}

			routingPrefix := routes[0].PathMatchCondition.(*PrefixMatchCondition).Prefix

			// There's no alternate forms for '/' :)
			if routingPrefix == "/" {
				continue
			}

			// Shallow copy the Route. TODO(jpeach) deep copying would be more robust.
			newRoute := *routes[0]

			// Now, make the original route handle '/foo' and the new route handle '/foo'.
			routes[0].PrefixRewrite = strings.TrimRight(routes[0].PrefixRewrite, "/")
			routes[0].PathMatchCondition = &PrefixMatchCondition{Prefix: prefix}

			newRoute.PrefixRewrite = routes[0].PrefixRewrite + "/"
			newRoute.PathMatchCondition = &PrefixMatchCondition{Prefix: prefix + "/"}

			// Since we trimmed trailing '/', it's possible that
			// we made the replacement empty. There's no such
			// thing as an empty rewrite; it's the same as
			// rewriting to '/'.
			if len(routes[0].PrefixRewrite) == 0 {
				routes[0].PrefixRewrite = "/"
			}

			expandedRoutes = append(expandedRoutes, &newRoute)
		case 2:
			// This group routes on both '/foo' and
			// '/foo/' so we can't add any implicit prefix
			// matches. This is why we didn't filter out
			// routes that don't have replacements earlier.
			continue
		default:
			// This can't happen unless there are routes
			// with duplicate prefix paths.
		}

	}

	return expandedRoutes
}

func getProtocol(service projcontour.Service, s *Service) (string, error) {
	// Determine the protocol to use to speak to this Cluster.
	var protocol string
	if service.Protocol != nil {
		protocol = *service.Protocol
		switch protocol {
		case "h2c", "h2", "tls":
		default:
			return "", fmt.Errorf("unsupported protocol: %v", protocol)
		}
	} else {
		protocol = s.Protocol
	}

	return protocol, nil
}

// determineSNI decides what the SNI should be on the request. It is configured via RequestHeadersPolicy.Host key.
// Policies set on service are used before policies set on a route. Otherwise the value of the externalService
// is used if the route is configured to proxy to an externalService type.
func determineSNI(routeRequestHeaders *HeadersPolicy, clusterRequestHeaders *HeadersPolicy, service *Service) string {

	// Service RequestHeadersPolicy take precedence
	if clusterRequestHeaders != nil {
		if clusterRequestHeaders.HostRewrite != "" {
			return clusterRequestHeaders.HostRewrite
		}
	}

	// Route RequestHeadersPolicy take precedence after service
	if routeRequestHeaders != nil {
		if routeRequestHeaders.HostRewrite != "" {
			return routeRequestHeaders.HostRewrite
		}
	}

	return service.ExternalName
}

func includeMatchConditionsIdentical(includes []projcontour.Include) bool {
	j := 0
	for i := 1; i < len(includes); i++ {
		// Now compare each include's set of conditions
		for _, cA := range includes[i].Conditions {
			for _, cB := range includes[j].Conditions {
				if (cA.Prefix == cB.Prefix) && equality.Semantic.DeepEqual(cA.Header, cB.Header) {
					return true
				}
			}
		}
		j++
	}
	return false
}

// isBlank indicates if a string contains nothing but blank characters.
func isBlank(s string) bool {
	return len(strings.TrimSpace(s)) == 0
}

// routeEnforceTLS determines if the route should redirect the user to a secure TLS listener
func routeEnforceTLS(enforceTLS, permitInsecure bool) bool {
	return enforceTLS && !permitInsecure
}
