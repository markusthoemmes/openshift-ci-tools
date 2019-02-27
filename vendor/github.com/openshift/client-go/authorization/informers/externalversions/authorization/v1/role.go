// This file was automatically generated by informer-gen

package v1

import (
	authorization_v1 "github.com/openshift/api/authorization/v1"
	versioned "github.com/openshift/client-go/authorization/clientset/versioned"
	internalinterfaces "github.com/openshift/client-go/authorization/informers/externalversions/internalinterfaces"
	v1 "github.com/openshift/client-go/authorization/listers/authorization/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
	time "time"
)

// RoleInformer provides access to a shared informer and lister for
// Roles.
type RoleInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1.RoleLister
}

type roleInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewRoleInformer constructs a new informer for Role type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewRoleInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredRoleInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredRoleInformer constructs a new informer for Role type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredRoleInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AuthorizationV1().Roles(namespace).List(options)
			},
			WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AuthorizationV1().Roles(namespace).Watch(options)
			},
		},
		&authorization_v1.Role{},
		resyncPeriod,
		indexers,
	)
}

func (f *roleInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredRoleInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *roleInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&authorization_v1.Role{}, f.defaultInformer)
}

func (f *roleInformer) Lister() v1.RoleLister {
	return v1.NewRoleLister(f.Informer().GetIndexer())
}
