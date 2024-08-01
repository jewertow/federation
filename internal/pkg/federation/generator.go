package federation

import (
	"fmt"
	"github.com/jewertow/federation/internal/api/federation/v1alpha1"
	"github.com/jewertow/federation/internal/pkg/common"
	"github.com/jewertow/federation/internal/pkg/config"
	"github.com/jewertow/federation/internal/pkg/xds/adss"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

var _ adss.RequestHandler = (*exportedServicesGenerator)(nil)

type exportedServicesGenerator struct {
	cfg             config.Federation
	typeUrl         string
	serviceInformer cache.SharedIndexInformer
}

func NewExportedServicesGenerator(cfg config.Federation, informerFactory informers.SharedInformerFactory) *exportedServicesGenerator {
	return &exportedServicesGenerator{
		cfg:             cfg,
		typeUrl:         "federation.istio-ecosystem.io/v1alpha1/ExportedService",
		serviceInformer: informerFactory.Core().V1().Services().Informer(),
	}
}

func (g exportedServicesGenerator) GetTypeUrl() string {
	return g.typeUrl
}

func (g exportedServicesGenerator) GenerateResponse() ([]*anypb.Any, error) {
	var serializedServices []*anypb.Any
	for _, obj := range g.serviceInformer.GetStore().List() {
		svc := obj.(*corev1.Service)
		if !common.MatchExportRules(svc, g.cfg.ExportedServiceSet.GetLabelSelectors()) {
			continue
		}
		exportedService := &v1alpha1.ExportedService{
			Name:      svc.Name,
			Namespace: svc.Namespace,
		}
		serializedExportedService := &anypb.Any{}
		if err := anypb.MarshalFrom(serializedExportedService, exportedService, proto.MarshalOptions{}); err != nil {
			return []*anypb.Any{}, fmt.Errorf("failed to serialize Gateway to protobuf message: %w", err)
		}
		serializedServices = append(serializedServices, serializedExportedService)
	}
	return serializedServices, nil
}