package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"text/template"

	"github.com/openshift/cluster-dns-operator/pkg/manifests"

	"github.com/sirupsen/logrus"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	operatorv1 "github.com/openshift/api/operator/v1"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const resolvConf = "/etc/resolv.conf"

var errInvalidNetworkUpstream = fmt.Errorf("The address field is mandatory for upstream of type Network, but was not provided")
var corefileTemplate = template.Must(template.New("Corefile").Funcs(template.FuncMap{
	"CoreDNSForwardingPolicy": coreDNSPolicy, "UpstreamResolver": coreDNSResolver,
}).Parse(`{{range .Servers -}}
# {{.Name}}
{{range .Zones}}{{.}}:5353 {{end}}{
    {{with .ForwardPlugin -}}
    prometheus 127.0.0.1:9153
    forward .{{range .Upstreams}} {{.}}{{end}} {
        policy {{ CoreDNSForwardingPolicy .Policy }}
    }
    {{- end}}
    errors
    log . {
        {{$.LogLevel}}
    }
    bufsize 512
    cache 900 {
        denial 9984 30
    }
}
{{end -}}
.:5353 {
    bufsize 512
    errors
    log . {
        {{.LogLevel}}
    }
    health {
        lameduck 20s
    }
    ready
    kubernetes {{.ClusterDomain}} in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
    }
    prometheus 127.0.0.1:9153
	{{- with .UpstreamResolvers }}
    forward .{{range .Upstreams}} {{UpstreamResolver .}}{{end}} {
        policy {{ CoreDNSForwardingPolicy .Policy }}
    }
	{{- end}}
    cache 900 {
        denial 9984 30
    }
    reload
}
`))

// ensureDNSConfigMap ensures that a configmap exists for a given DNS.
func (r *reconciler) ensureDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (bool, *corev1.ConfigMap, error) {
	haveCM, current, err := r.currentDNSConfigMap(dns)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get configmap: %v", err)
	}
	desired, err := desiredDNSConfigMap(dns, clusterDomain)
	if err != nil {
		return haveCM, current, fmt.Errorf("failed to build configmap: %v", err)
	}

	switch {
	case !haveCM:
		if err := r.client.Create(context.TODO(), desired); err != nil {
			return false, nil, fmt.Errorf("failed to create configmap: %v", err)
		}
		logrus.Infof("created configmap: %s", desired.Name)
		return r.currentDNSConfigMap(dns)
	case haveCM:
		if updated, err := r.updateDNSConfigMap(current, desired); err != nil {
			return true, current, err
		} else if updated {
			return r.currentDNSConfigMap(dns)
		}
	}
	return true, current, nil
}

func (r *reconciler) currentDNSConfigMap(dns *operatorv1.DNS) (bool, *corev1.ConfigMap, error) {
	current := &corev1.ConfigMap{}
	err := r.client.Get(context.TODO(), DNSConfigMapName(dns), current)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, current, nil
}

func desiredDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (*corev1.ConfigMap, error) {
	if len(clusterDomain) == 0 {
		clusterDomain = "cluster.local"
	}

	upstreamResolvers := operatorv1.UpstreamResolvers{
		Upstreams: []operatorv1.Upstream{
			{
				Type: operatorv1.SystemResolveConfType,
			},
		},
		Policy: operatorv1.SequentialForwardingPolicy,
	}

	if len(dns.Spec.UpstreamResolvers.Upstreams) > 0 {
		upstreamResolvers.Upstreams = dns.Spec.UpstreamResolvers.Upstreams
		for _, upstream := range upstreamResolvers.Upstreams {
			if upstream.Type == operatorv1.NetworkResolverType && upstream.Address == "" {
				return nil, errInvalidNetworkUpstream
			}
		}
	}

	if dns.Spec.UpstreamResolvers.Policy != "" {
		upstreamResolvers.Policy = dns.Spec.UpstreamResolvers.Policy
	}

	corefileParameters := struct {
		ClusterDomain     string
		Servers           interface{}
		UpstreamResolvers operatorv1.UpstreamResolvers
		PolicyStr         func(policy operatorv1.ForwardingPolicy) string
		LogLevel          string
	}{
		ClusterDomain:     clusterDomain,
		Servers:           dns.Spec.Servers,
		UpstreamResolvers: upstreamResolvers,
		PolicyStr:         coreDNSPolicy,
		LogLevel:          coreDNSLogLevel(dns),
	}
	corefile := new(bytes.Buffer)
	if err := corefileTemplate.Execute(corefile, corefileParameters); err != nil {
		return nil, err
	}

	name := DNSConfigMapName(dns)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
			Labels: map[string]string{
				manifests.OwningDNSLabel: DNSDaemonSetLabel(dns),
			},
		},
		Data: map[string]string{
			"Corefile": corefile.String(),
		},
	}
	cm.SetOwnerReferences([]metav1.OwnerReference{dnsOwnerRef(dns)})

	return cm, nil
}

func (r *reconciler) updateDNSConfigMap(current, desired *corev1.ConfigMap) (bool, error) {
	changed, updated := corefileChanged(current, desired)
	if !changed {
		return false, nil
	}

	// Diff before updating because the client may mutate the object.
	diff := cmp.Diff(current, updated, cmpopts.EquateEmpty())
	if err := r.client.Update(context.TODO(), updated); err != nil {
		return false, fmt.Errorf("failed to update configmap: %v", err)
	}
	logrus.Infof("updated configmap %s/%s: %v", updated.Namespace, updated.Name, diff)
	return true, nil
}

func corefileChanged(current, expected *corev1.ConfigMap) (bool, *corev1.ConfigMap) {
	if cmp.Equal(current.Data, expected.Data, cmpopts.EquateEmpty()) {
		return false, current
	}
	updated := current.DeepCopy()
	updated.Data = expected.Data
	return true, updated
}

func coreDNSResolver(upstream operatorv1.Upstream) (string, error) {
	if upstream.Type == operatorv1.NetworkResolverType {
		if upstream.Address == "" {
			return "", errInvalidNetworkUpstream
		}
		if upstream.Port > 0 {
			return net.JoinHostPort(upstream.Address, fmt.Sprintf("%d", upstream.Port)), nil
		} else {
			return upstream.Address, nil
		}
	}
	return resolvConf, nil
}

func coreDNSPolicy(policy operatorv1.ForwardingPolicy) string {
	switch policy {
	case operatorv1.RandomForwardingPolicy:
		return "random"
	case operatorv1.RoundRobinForwardingPolicy:
		return "round_robin"
	case operatorv1.SequentialForwardingPolicy:
		return "sequential"
	}
	return "random"
}

func coreDNSLogLevel(dns *operatorv1.DNS) string {
	switch dns.Spec.LogLevel {
	case operatorv1.DNSLogLevelNormal:
		return "class error"
	case operatorv1.DNSLogLevelDebug:
		return "class denial error"
	case operatorv1.DNSLogLevelTrace:
		return "class all"
	}
	return "class error"
}
