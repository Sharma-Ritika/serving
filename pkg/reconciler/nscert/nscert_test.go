/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nscert

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sync/errgroup"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	. "knative.dev/pkg/reconciler/testing"
	"knative.dev/pkg/system"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/reconciler/nscert/config"
	"knative.dev/serving/pkg/reconciler/nscert/resources/names"
	routecfg "knative.dev/serving/pkg/reconciler/route/config"

	fakekubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	fakensinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/namespace/fake"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	fakeservingclient "knative.dev/serving/pkg/client/injection/client/fake"
	fakecertinformer "knative.dev/serving/pkg/client/injection/informers/networking/v1alpha1/certificate/fake"

	_ "knative.dev/pkg/system/testing"

	. "knative.dev/serving/pkg/reconciler/testing/v1"
)

const testCertClass = "dns-01.rocks"

var (
	wildcardDNSNames      = []string{"*.foo.example.com"}
	defaultCertName       = names.WildcardCertificate(wildcardDNSNames[0])
	defaultDomainTemplate = "{{.Name}}.{{.Namespace}}.{{.Domain}}"
	defaultDomain         = "example.com"
)

func newTestSetup(t *testing.T, configs ...*corev1.ConfigMap) (
	context.Context, context.CancelFunc, chan *v1alpha1.Certificate, *configmap.ManualWatcher) {
	t.Helper()

	ctx, ccl, ifs := SetupFakeContextWithCancel(t)
	wf, err := controller.RunInformers(ctx.Done(), ifs...)
	if err != nil {
		t.Fatalf("Error starting informers: %v", err)
	}
	cancel := func() {
		ccl()
		wf()
	}

	configMapWatcher := &configmap.ManualWatcher{Namespace: system.Namespace()}

	ctl := NewController(ctx, configMapWatcher)

	cms := []*corev1.ConfigMap{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "config-network",
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"domainTemplate": defaultDomainTemplate,
			"autoTLS":        "true",
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      routecfg.DomainConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"example.com": "",
		},
	}}
	cms = append(cms, configs...)

	for _, cfg := range cms {
		configMapWatcher.OnChange(cfg)
	}
	if err := configMapWatcher.Start(ctx.Done()); err != nil {
		t.Fatalf("failed to start config manager: %v", err)
	}

	certEvents := make(chan *v1alpha1.Certificate)
	fakecertinformer.Get(ctx).Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Namespace")),
		Handler: controller.HandleAll(func(obj interface{}) {
			certEvents <- obj.(*v1alpha1.Certificate)
		}),
	})

	var eg errgroup.Group
	eg.Go(func() error { return ctl.Run(1, ctx.Done()) })
	return ctx, func() {
		cancel()
		eg.Wait()
	}, certEvents, configMapWatcher
}

func TestNewController(t *testing.T) {
	ctx, _ := SetupFakeContext(t)

	configMapWatcher := configmap.NewStaticWatcher(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"DomainTemplate": defaultDomainTemplate,
		}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      routecfg.DomainConfigName,
				Namespace: system.Namespace(),
			},
			Data: map[string]string{
				"example.com": "",
			}},
	)

	c := NewController(ctx, configMapWatcher)

	if c == nil {
		t.Fatal("Expected NewController to return a non-nil value")
	}
}

// This is heavily based on the way the OpenShift Ingress controller tests its reconciliation method.
func TestReconcile(t *testing.T) {
	table := TableTest{{
		Name: "bad workqueue key",
		Key:  "too/many/parts",
	}, {
		Name: "key not found",
		Key:  "foo/not-found",
	}, {
		Name:                    "create Knative certificate for namespace",
		SkipNamespaceValidation: true,
		Objects: []runtime.Object{
			kubeNamespace("foo"),
		},
		WantCreates: []runtime.Object{
			knCert(kubeNamespace("foo")),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Created", "Created Knative Certificate %s/%s", "foo", defaultCertName),
		},
		Key: "foo",
	}, {
		Name:                    "create Knative certificate for namespace with explicitly enabled",
		SkipNamespaceValidation: true,
		Objects: []runtime.Object{
			kubeNamespaceWithDisableLabelValue("foo", "false"),
		},
		WantCreates: []runtime.Object{
			knCert(kubeNamespace("foo")),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Created", "Created Knative Certificate %s/%s", "foo", defaultCertName),
		},
		Key: "foo",
	}, {
		Name: "certificate not created for excluded namespace",
		Key:  "foo",
		Objects: []runtime.Object{
			kubeNamespaceWithDisableLabelValue("foo", "true"),
		},
	}, {
		Name:                    "certificate creation failed",
		Key:                     "foo",
		WantErr:                 true,
		SkipNamespaceValidation: true,
		Objects: []runtime.Object{
			kubeNamespace("foo"),
		},
		WantCreates: []runtime.Object{
			knCert(kubeNamespace("foo")),
		},
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("create", "certificates"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "CreationFailed", "Failed to create Knative certificate %s/%s: inducing failure for create certificates", "foo", defaultCertName),
			Eventf(corev1.EventTypeWarning, "InternalError", "failed to create namespace certificate: inducing failure for create certificates"),
		},
	}, {
		Name: "disabling namespace cert feature deletes the cert",
		Key:  "foo",
		Objects: []runtime.Object{
			kubeNamespaceWithDisableLabelValue("foo", "true"),
			knCert(kubeNamespace("foo")),
		},
		SkipNamespaceValidation: true,
		WantDeletes: []clientgotesting.DeleteActionImpl{{
			ActionImpl: clientgotesting.ActionImpl{
				Namespace: "foo",
				Verb:      "delete",
				Resource: schema.GroupVersionResource{
					Group:    "networking.internal.knative.dev",
					Version:  "v1alpha1",
					Resource: "certificates",
				},
			},
			Name: "foo.example.com",
		}},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Deleted", "Deleted Knative Certificate %s/%s", "foo", defaultCertName),
		},
	}}

	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher) controller.Reconciler {
		return &reconciler{
			client:              servingclient.Get(ctx),
			recorder:            controller.GetEventRecorder(ctx),
			knCertificateLister: listers.GetKnCertificateLister(),
			nsLister:            listers.GetNamespaceLister(),
			configStore: &testConfigStore{
				config: &config.Config{
					Network: networkConfig(),
					Domain:  domainConfig(),
				},
			},
		}
	}))
}

func TestUpdateDomainTemplate(t *testing.T) {
	netCfg := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"autoTLS": "Enabled",
		},
	}
	ctx, cancel, certEvents, watcher := newTestSetup(t, netCfg)
	defer cancel()

	namespace := kubeNamespace("testns")
	fakekubeclient.Get(ctx).CoreV1().Namespaces().Create(namespace)
	fakensinformer.Get(ctx).Informer().GetIndexer().Add(namespace)

	want := []string{fmt.Sprintf("*.%s.%s", namespace.Name, routecfg.DefaultDomain)}
	cert := <-certEvents
	if diff := cmp.Diff(want, cert.Spec.DNSNames); diff != "" {
		t.Errorf("DNSNames (-want, +got) = %s", diff)
	}

	// Update the domain template to something matched by the existing DNSName
	netCfg = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"domainTemplate": "{{.Name}}-suffix.{{.Namespace}}.{{.Domain}}",
			"autoTLS":        "Enabled",
		},
	}
	watcher.OnChange(netCfg)

	// Since no new names should be added nothing should change
	select {
	case <-certEvents:
		t.Error("Unexpected event")
	case <-time.After(100 * time.Millisecond):
	}

	// Update the domain template to something not matched by the existing DNSName
	netCfg = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"domainTemplate": "{{.Name}}.subdomain.{{.Namespace}}.{{.Domain}}",
			"autoTLS":        "Enabled",
		},
	}
	watcher.OnChange(netCfg)

	// A new domain format not matched by the existing certificate should update the DNSName
	want = []string{fmt.Sprintf("*.subdomain.%s.%s", namespace.Name, routecfg.DefaultDomain)}
	cert = <-certEvents
	if diff := cmp.Diff(want, cert.Spec.DNSNames); diff != "" {
		t.Errorf("DNSNames (-want, +got) = %s", diff)
	}

	// Invalid domain template for wildcard certs
	netCfg = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"domainTemplate": "{{.Namespace}}.{{.Name}}.{{.Domain}}",
			"autoTLS":        "Enabled",
		},
	}
	watcher.OnChange(netCfg)

	// With an invalid domain template nothing change
	select {
	case <-certEvents:
		t.Error("Unexpected event")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestChangeDefaultDomain(t *testing.T) {
	netCfg := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      network.ConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"autoTLS": "Enabled",
		},
	}

	ctx, cancel, certEvents, watcher := newTestSetup(t, netCfg)
	defer cancel()

	namespace := kubeNamespace("testns")
	fakekubeclient.Get(ctx).CoreV1().Namespaces().Create(namespace)
	fakensinformer.Get(ctx).Informer().GetIndexer().Add(namespace)

	// The certificate should be created with the default domain.
	cert := <-certEvents
	if got, want := cert.Spec.DNSNames[0], "*.testns.example.com"; got != want {
		t.Errorf("DNSName[0] = %s, want %s", got, want)
	}

	// Change the domain settings.
	domCfg := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routecfg.DomainConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"example.net": "",
		},
	}
	watcher.OnChange(domCfg)

	// The certificate should be updated with the new domain.
	cert2 := <-certEvents
	if got, want := cert2.Spec.DNSNames[0], "*.testns.example.net"; got != want {
		t.Errorf("DNSName[0] = %s, want %s", got, want)
	}

	// Assert we have exactly one certificate.
	certs, _ := fakeservingclient.Get(ctx).NetworkingV1alpha1().Certificates(namespace.Name).List(metav1.ListOptions{})
	if len(certs.Items) > 1 {
		t.Errorf("Expected 1 certificate, got %d.", len(certs.Items))
	}
}

func TestDomainConfigDomain(t *testing.T) {
	const ns = "testns"

	tests := []struct {
		name         string
		domainCfg    map[string]string
		wantCertName string
		wantDNSName  string
	}{{
		name: "default domain",
		domainCfg: map[string]string{
			"other.com": "selector:\n app: dev",
		},
		wantCertName: "testns.example.com",
		wantDNSName:  "*.testns.example.com",
	}, {
		name: "default domain",
		domainCfg: map[string]string{
			"default.com": "",
		},
		wantCertName: "testns.default.com",
		wantDNSName:  "*.testns.default.com",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			domCfg := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      routecfg.DomainConfigName,
					Namespace: system.Namespace(),
				},
				Data: test.domainCfg,
			}
			netCfg := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      network.ConfigName,
					Namespace: system.Namespace(),
				},
				Data: map[string]string{
					"autoTLS": "Enabled",
				},
			}

			ctx, ccl, ifs := SetupFakeContextWithCancel(t)
			wf, err := controller.RunInformers(ctx.Done(), ifs...)
			if err != nil {
				t.Fatalf("Error starting informers: %v", err)
			}
			defer func() {
				ccl()
				wf()
			}()

			cmw := configmap.NewStaticWatcher(domCfg, netCfg)
			configStore := config.NewStore(logging.FromContext(ctx).Named("config-store"))
			configStore.WatchConfigs(cmw)

			r := &reconciler{
				client:              servingclient.Get(ctx),
				recorder:            controller.GetEventRecorder(ctx),
				configStore:         configStore,
				nsLister:            fakensinformer.Get(ctx).Lister(),
				knCertificateLister: fakecertinformer.Get(ctx).Lister(),
			}

			namespace := kubeNamespace(ns)
			fakekubeclient.Get(ctx).CoreV1().Namespaces().Create(namespace)
			fakensinformer.Get(ctx).Informer().GetIndexer().Add(namespace)

			r.Reconcile(ctx, ns)

			cert, err := fakeservingclient.Get(ctx).NetworkingV1alpha1().Certificates(ns).Get(test.wantCertName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Could not get certificate: %v", err)
			}
			if got, want := cert.Spec.DNSNames[0], test.wantDNSName; got != want {
				t.Errorf("DNSName[0] = %s, want %s", got, want)
			}
		})
	}
}

type testConfigStore struct {
	config *config.Config
}

func (t *testConfigStore) ToContext(ctx context.Context) context.Context {
	return config.ToContext(ctx, t.config)
}

var _ pkgreconciler.ConfigStore = (*testConfigStore)(nil)

func knCert(namespace *corev1.Namespace) *v1alpha1.Certificate {
	return knCertWithStatus(namespace, &v1alpha1.CertificateStatus{})
}

func knCertWithStatus(namespace *corev1.Namespace, status *v1alpha1.CertificateStatus) *v1alpha1.Certificate {
	return &v1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:            defaultCertName,
			Namespace:       namespace.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(namespace, corev1.SchemeGroupVersion.WithKind("Namespace"))},
			Annotations: map[string]string{
				networking.CertificateClassAnnotationKey: testCertClass,
			},
			Labels: map[string]string{
				networking.WildcardCertDomainLabelKey: defaultDomain,
			},
		},
		Spec: v1alpha1.CertificateSpec{
			DNSNames:   wildcardDNSNames,
			SecretName: defaultCertName,
		},
		Status: *status,
	}
}

func kubeNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func kubeNamespaceWithDisableLabelValue(name, value string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				networking.DisableWildcardCertLabelKey: value,
			},
		},
	}
}

func networkConfig() *network.Config {
	return &network.Config{
		DomainTemplate:          defaultDomainTemplate,
		AutoTLS:                 true,
		DefaultCertificateClass: testCertClass,
	}
}

func domainConfig() *routecfg.Domain {
	domainConfig := &routecfg.Domain{
		Domains: map[string]*routecfg.LabelSelector{
			"example.com": {},
		},
	}
	return domainConfig
}