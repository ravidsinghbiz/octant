package overview

import (
	"context"
	"fmt"
	"path"
	"sort"
	"sync"

	"github.com/pkg/errors"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kcache "k8s.io/client-go/tools/cache"

	"github.com/heptio/developer-dash/internal/config"
	"github.com/heptio/developer-dash/internal/describer"
	"github.com/heptio/developer-dash/internal/link"
	"github.com/heptio/developer-dash/internal/log"
	"github.com/heptio/developer-dash/internal/modules/overview/printer"
	"github.com/heptio/developer-dash/internal/modules/overview/resourceviewer"
	"github.com/heptio/developer-dash/internal/modules/overview/yamlviewer"
	"github.com/heptio/developer-dash/internal/objectstore"
	"github.com/heptio/developer-dash/internal/queryer"
	"github.com/heptio/developer-dash/pkg/objectstoreutil"
	"github.com/heptio/developer-dash/pkg/view/component"
)

func customResourceDefinitionNames(ctx context.Context, o objectstore.ObjectStore) ([]string, error) {
	key := objectstoreutil.Key{
		APIVersion: "apiextensions.k8s.io/v1beta1",
		Kind:       "CustomResourceDefinition",
	}

	if err := o.HasAccess(key, "list"); err != nil {
		return []string{}, nil
	}

	rawList, err := o.List(ctx, key)
	if err != nil {
		return nil, errors.Wrap(err, "listing CRDs")
	}

	var list []string

	for _, object := range rawList {
		crd := &apiextv1beta1.CustomResourceDefinition{}

		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, crd); err != nil {
			return nil, errors.Wrap(err, "crd conversion failed")
		}

		list = append(list, crd.Name)
	}

	return list, nil
}

func customResourceDefinition(ctx context.Context, name string, o objectstore.ObjectStore) (*apiextv1beta1.CustomResourceDefinition, error) {
	key := objectstoreutil.Key{
		APIVersion: "apiextensions.k8s.io/v1beta1",
		Kind:       "CustomResourceDefinition",
		Name:       name,
	}

	crd := &apiextv1beta1.CustomResourceDefinition{}
	if err := objectstore.GetAs(ctx, o, key, crd); err != nil {
		return nil, errors.Wrap(err, "get CRD from object store")
	}

	return crd, nil
}

type crdSectionDescriber struct {
	describers map[string]describer.Describer
	path       string
	title      string

	mu sync.Mutex
}

var _ describer.Describer = (*crdSectionDescriber)(nil)

func newCRDSectionDescriber(p, title string) *crdSectionDescriber {
	return &crdSectionDescriber{
		describers: make(map[string]describer.Describer),
		path:       p,
		title:      title,
	}
}

func (csd *crdSectionDescriber) Add(name string, describer describer.Describer) {
	csd.mu.Lock()
	defer csd.mu.Unlock()

	csd.describers[name] = describer
}

func (csd *crdSectionDescriber) Remove(name string) {
	csd.mu.Lock()
	defer csd.mu.Unlock()

	delete(csd.describers, name)
}

func (csd *crdSectionDescriber) Describe(ctx context.Context, prefix, namespace string, options describer.Options) (component.ContentResponse, error) {
	csd.mu.Lock()
	defer csd.mu.Unlock()

	var names []string
	for name := range csd.describers {
		names = append(names, name)
	}

	sort.Strings(names)

	list := component.NewList("Custom Resources", nil)

	for _, name := range names {
		resp, err := csd.describers[name].Describe(ctx, prefix, namespace, options)
		if err != nil {
			return emptyContentResponse, err
		}

		for i := range resp.Components {
			if nestedList, ok := resp.Components[i].(*component.List); ok {
				for i := range nestedList.Config.Items {
					item := nestedList.Config.Items[i]
					if !item.IsEmpty() {
						list.Add(item)
					}
				}
			}
		}
	}

	cr := component.ContentResponse{
		Components: []component.Component{list},
		Title:      component.TitleFromString(csd.title),
	}

	return cr, nil
}

func (csd *crdSectionDescriber) PathFilters() []describer.PathFilter {
	return []describer.PathFilter{
		*describer.NewPathFilter(csd.path, csd),
	}
}

type crdListPrinter func(
	crdName string,
	crd *apiextv1beta1.CustomResourceDefinition,
	objects []*unstructured.Unstructured,
	linkGenerator link.Interface) (component.Component, error)

type crdListDescriptionOption func(*crdListDescriber)

type crdListDescriber struct {
	name    string
	path    string
	printer crdListPrinter
}

var _ describer.Describer = (*crdListDescriber)(nil)

func newCRDListDescriber(name, path string, options ...crdListDescriptionOption) *crdListDescriber {
	d := &crdListDescriber{
		name:    name,
		path:    path,
		printer: printer.CustomResourceListHandler,
	}

	for _, option := range options {
		option(d)
	}

	return d
}

func (cld *crdListDescriber) Describe(ctx context.Context, prefix, namespace string, options describer.Options) (component.ContentResponse, error) {
	objectStore := options.ObjectStore()
	crd, err := customResourceDefinition(ctx, cld.name, objectStore)
	if err != nil {
		return emptyContentResponse, err
	}

	objects, err := listCustomResources(ctx, crd, namespace, objectStore, options.LabelSet)
	if err != nil {
		return emptyContentResponse, err
	}

	table, err := cld.printer(cld.name, crd, objects, options.Link)
	if err != nil {
		return emptyContentResponse, err
	}

	list := component.NewList(fmt.Sprintf("Custom Resources / %s", cld.name), []component.Component{
		table,
	})

	return component.ContentResponse{
		Components: []component.Component{list},
	}, nil
}

func listCustomResources(
	ctx context.Context,
	crd *apiextv1beta1.CustomResourceDefinition,
	namespace string,
	o objectstore.ObjectStore,
	selector *labels.Set) ([]*unstructured.Unstructured, error) {
	if crd == nil {
		return nil, errors.New("crd is nil")
	}
	gvk := schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: crd.Spec.Version,
		Kind:    crd.Spec.Names.Kind,
	}

	apiVersion, kind := gvk.ToAPIVersionAndKind()

	key := objectstoreutil.Key{
		Namespace:  namespace,
		APIVersion: apiVersion,
		Kind:       kind,
		Selector:   selector,
	}

	if err := o.HasAccess(key, "list"); err != nil {
		return []*unstructured.Unstructured{}, nil
	}

	objects, err := o.List(ctx, key)
	if err != nil {
		return nil, errors.Wrapf(err, "listing custom resources for %q", crd.Name)
	}

	return objects, nil
}

func (cld *crdListDescriber) PathFilters() []describer.PathFilter {
	return []describer.PathFilter{
		*describer.NewPathFilter(cld.path, cld),
	}
}

type crdPrinter func(ctx context.Context, crd *apiextv1beta1.CustomResourceDefinition, object *unstructured.Unstructured, options printer.Options) (component.Component, error)
type resourceViewerPrinter func(ctx context.Context, object *unstructured.Unstructured, dashConfig config.Dash, q queryer.Queryer) (component.Component, error)
type yamlPrinter func(runtime.Object) (*component.YAML, error)

type crdDescriberOption func(*crdDescriber)

type crdDescriber struct {
	path                  string
	name                  string
	summaryPrinter        crdPrinter
	resourceViewerPrinter resourceViewerPrinter
	yamlPrinter           yamlPrinter
}

var _ describer.Describer = (*crdDescriber)(nil)

func newCRDDescriber(name, path string, options ...crdDescriberOption) *crdDescriber {
	d := &crdDescriber{
		path:                  path,
		name:                  name,
		summaryPrinter:        printer.CustomResourceHandler,
		resourceViewerPrinter: createCRDResourceViewer,
		yamlPrinter:           yamlviewer.ToComponent,
	}

	for _, option := range options {
		option(d)
	}

	return d
}

func (cd *crdDescriber) Describe(ctx context.Context, prefix, namespace string, options describer.Options) (component.ContentResponse, error) {
	objectStore := options.ObjectStore()
	crd, err := customResourceDefinition(ctx, cd.name, objectStore)
	if err != nil {
		return emptyContentResponse, err
	}

	gvk := schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: crd.Spec.Version,
		Kind:    crd.Spec.Names.Kind,
	}

	apiVersion, kind := gvk.ToAPIVersionAndKind()

	key := objectstoreutil.Key{
		Namespace:  namespace,
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       options.Fields["name"],
	}

	object, err := objectStore.Get(ctx, key)
	if err != nil {
		return emptyContentResponse, err
	}

	// TODO: shouldn't use the nil, should use the error
	if object == nil {
		return emptyContentResponse, err
	}

	title := component.Title(
		options.Link.ForCustomResourceDefinition(cd.name, namespace),
		component.NewText(object.GetName()))

	cr := component.NewContentResponse(title)

	linkGenerator, err := link.NewFromDashConfig(options)
	if err != nil {
		return emptyContentResponse, err
	}

	printOptions := printer.Options{
		DashConfig: options,
		Link:       linkGenerator,
	}

	summary, err := cd.summaryPrinter(ctx, crd, object, printOptions)
	if err != nil {
		return emptyContentResponse, err
	}
	summary.SetAccessor("summary")

	cr.Add(summary)

	resourceViewerComponent, err := cd.resourceViewerPrinter(ctx, object, options, options.Queryer)
	if err != nil {
		return emptyContentResponse, err
	}

	resourceViewerComponent.SetAccessor("resourceViewer")
	cr.Add(resourceViewerComponent)

	yvComponent, err := cd.yamlPrinter(object)
	if err != nil {
		return emptyContentResponse, err
	}

	yvComponent.SetAccessor("yaml")
	cr.Add(yvComponent)

	pluginPrinter := options.PluginManager()
	tabs, err := pluginPrinter.Tabs(object)
	if err != nil {
		return emptyContentResponse, errors.Wrap(err, "getting tabs from plugins")
	}

	for _, tab := range tabs {
		tab.Contents.SetAccessor(tab.Name)
		cr.Add(&tab.Contents)
	}

	return *cr, nil
}

func (cd *crdDescriber) PathFilters() []describer.PathFilter {
	return []describer.PathFilter{
		*describer.NewPathFilter(cd.path, cd),
	}
}

func createCRDResourceViewer(ctx context.Context, object *unstructured.Unstructured, dashConfig config.Dash, q queryer.Queryer) (component.Component, error) {
	rv, err := resourceviewer.New(dashConfig, resourceviewer.WithDefaultQueryer(q))
	if err != nil {
		return nil, err
	}

	return rv.Visit(ctx, object)
}

type objectHandler func(ctx context.Context, object *unstructured.Unstructured)

func watchCRDs(ctx context.Context, o objectstore.ObjectStore, crdAddFunc, crdDeleteFunc objectHandler) {
	handler := &kcache.ResourceEventHandlerFuncs{}

	if crdAddFunc != nil {
		handler.AddFunc = func(object interface{}) {
			u, ok := object.(*unstructured.Unstructured)
			if ok {
				crdAddFunc(ctx, u)
			}
		}
	}

	if crdDeleteFunc != nil {
		handler.DeleteFunc = func(object interface{}) {
			u, ok := object.(*unstructured.Unstructured)
			if ok {
				crdDeleteFunc(ctx, u)
			}
		}
	}

	key := objectstoreutil.Key{
		APIVersion: "apiextensions.k8s.io/v1beta1",
		Kind:       "CustomResourceDefinition",
	}

	logger := log.From(ctx)
	if err := o.Watch(ctx, key, handler); err != nil {
		logger.Errorf("crd watcher has failed: %v", err)
	}
}

func addCRD(ctx context.Context, name string, pm *describer.PathMatcher, sectionDescriber *crdSectionDescriber) {
	logger := log.From(ctx)
	logger.With("crd-name", name).Debugf("adding CRD")

	cld := newCRDListDescriber(name, crdListPath(name))

	sectionDescriber.Add(name, cld)

	for _, pf := range cld.PathFilters() {
		pm.Register(ctx, pf)
	}

	cd := newCRDDescriber(name, crdObjectPath(name))
	for _, pf := range cd.PathFilters() {
		pm.Register(ctx, pf)
	}
}

func deleteCRD(ctx context.Context, name string, pm *describer.PathMatcher, sectionDescriber *crdSectionDescriber) {
	logger := log.From(ctx)
	logger.With("crd-name", name).Debugf("deleting CRD")

	pm.Deregister(ctx, crdListPath(name))
	pm.Deregister(ctx, crdObjectPath(name))

	sectionDescriber.Remove(name)

}

func crdListPath(name string) string {
	return path.Join("/custom-resources", name)
}

func crdObjectPath(name string) string {
	return path.Join(crdListPath(name), describer.ResourceNameRegex)
}