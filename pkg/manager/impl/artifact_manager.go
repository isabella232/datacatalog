package impl

import (
	"context"
	"time"

	"github.com/lyft/datacatalog/pkg/errors"
	"github.com/lyft/datacatalog/pkg/manager/impl/validators"
	"github.com/lyft/datacatalog/pkg/manager/interfaces"
	"github.com/lyft/datacatalog/pkg/repositories"
	datacatalog "github.com/lyft/datacatalog/protos/gen"

	"github.com/lyft/datacatalog/pkg/repositories/models"
	"github.com/lyft/datacatalog/pkg/repositories/transformers"

	"github.com/lyft/flytestdlib/contextutils"
	"github.com/lyft/flytestdlib/promutils"
	"github.com/lyft/flytestdlib/promutils/labeled"
	"github.com/lyft/flytestdlib/storage"
	"google.golang.org/grpc/codes"
)

type artifactMetrics struct {
	scope                    promutils.Scope
	createSuccessCounter     labeled.Counter
	createFailureCounter     labeled.Counter
	getSuccessCounter        labeled.Counter
	getFailureCounter        labeled.Counter
	createDataFailureCounter labeled.Counter
	createDataSuccessCounter labeled.Counter
	transformerErrorCounter  labeled.Counter
	validationErrorCounter   labeled.Counter
	createResponseTime       labeled.StopWatch
	getResponseTime          labeled.StopWatch
}

type artifactManager struct {
	repo          repositories.RepositoryInterface
	artifactStore ArtifactDataStore
	systemMetrics artifactMetrics
}

// Create an Artifact along with the associated ArtifactData. The ArtifactData will be stored in an offloaded location.
func (m *artifactManager) CreateArtifact(ctx context.Context, request datacatalog.CreateArtifactRequest) (*datacatalog.CreateArtifactResponse, error) {
	timer := m.systemMetrics.createResponseTime.Start(ctx)
	defer timer.Stop()

	artifact := request.Artifact
	err := validators.ValidateArtifact(artifact)
	if err != nil {
		m.systemMetrics.validationErrorCounter.Inc(ctx)
		return nil, err
	}

	ctx = contextutils.WithProjectDomain(ctx, artifact.Dataset.Project, artifact.Dataset.Domain)
	datasetKey := transformers.FromDatasetID(*artifact.Dataset)

	// The dataset must exist for the artifact, let's verify that first
	_, err = m.repo.DatasetRepo().Get(ctx, datasetKey)
	if err != nil {
		m.systemMetrics.createFailureCounter.Inc(ctx)
		return nil, err
	}

	// create Artifact Data offloaded storage files
	artifactDataModels := make([]models.ArtifactData, len(request.Artifact.Data))
	for i, artifactData := range request.Artifact.Data {
		dataLocation, err := m.artifactStore.PutData(ctx, *artifact, *artifactData)
		if err != nil {
			m.systemMetrics.createDataFailureCounter.Inc(ctx)
			m.systemMetrics.createFailureCounter.Inc(ctx)
			return nil, err
		}

		artifactDataModels[i].Name = artifactData.Name
		artifactDataModels[i].Location = dataLocation.String()
		m.systemMetrics.createDataSuccessCounter.Inc(ctx)
	}

	artifactModel, err := transformers.CreateArtifactModel(request, artifactDataModels)
	if err != nil {
		m.systemMetrics.transformerErrorCounter.Inc(ctx)
		return nil, err
	}

	err = m.repo.ArtifactRepo().Create(ctx, artifactModel)
	if err != nil {
		m.systemMetrics.createFailureCounter.Inc(ctx)
		return nil, err
	}

	m.systemMetrics.createSuccessCounter.Inc(ctx)
	return &datacatalog.CreateArtifactResponse{}, nil
}

// Get the Artifact and its associated ArtifactData. The request can query by ArtifactID or TagName.
func (m *artifactManager) GetArtifact(ctx context.Context, request datacatalog.GetArtifactRequest) (*datacatalog.GetArtifactResponse, error) {
	datasetID := request.Dataset
	err := validators.ValidateGetArtifactRequest(request)
	if err != nil {
		m.systemMetrics.validationErrorCounter.Inc(ctx)
		return nil, err
	}

	ctx = contextutils.WithProjectDomain(ctx, datasetID.Project, datasetID.Domain)
	var artifactModel models.Artifact
	switch request.QueryHandle.(type) {
	case *datacatalog.GetArtifactRequest_ArtifactId:
		artifactKey := transformers.ToArtifactKey(*datasetID, request.GetArtifactId())
		artifactModel, err = m.repo.ArtifactRepo().Get(ctx, artifactKey)

		if err != nil {
			m.systemMetrics.getFailureCounter.Inc(ctx)
			return nil, err
		}
	case *datacatalog.GetArtifactRequest_TagName:
		tagKey := transformers.ToTagKey(*datasetID, request.GetTagName())
		tag, err := m.repo.TagRepo().Get(ctx, tagKey)

		if err != nil {
			m.systemMetrics.getFailureCounter.Inc(ctx)
			return nil, err
		}

		artifactModel = tag.Artifact
	}

	if len(artifactModel.ArtifactData) == 0 {
		return nil, errors.NewDataCatalogErrorf(codes.Internal, "artifact [%+v] does not have artifact data associated", request)
	}

	artifact, err := transformers.FromArtifactModel(artifactModel)
	if err != nil {
		m.systemMetrics.transformerErrorCounter.Inc(ctx)
		return nil, err
	}

	artifactDataList := make([]*datacatalog.ArtifactData, len(artifactModel.ArtifactData))
	for i, artifactData := range artifactModel.ArtifactData {
		value, err := m.artifactStore.GetData(ctx, artifactData)
		if err != nil {
			return nil, err
		}

		artifactDataList[i] = &datacatalog.ArtifactData{
			Name:  artifactData.Name,
			Value: value,
		}
	}
	artifact.Data = artifactDataList

	m.systemMetrics.getSuccessCounter.Inc(ctx)
	return &datacatalog.GetArtifactResponse{
		Artifact: &artifact,
	}, nil
}

func NewArtifactManager(repo repositories.RepositoryInterface, store *storage.DataStore, storagePrefix storage.DataReference, artifactScope promutils.Scope) interfaces.ArtifactManager {
	artifactMetrics := artifactMetrics{
		scope:                    artifactScope,
		createResponseTime:       labeled.NewStopWatch("create_artifact_duration", "The duration of the create artifact calls.", time.Millisecond, artifactScope, labeled.EmitUnlabeledMetric),
		getResponseTime:          labeled.NewStopWatch("get_artifact_duration", "The duration of the get artifact calls.", time.Millisecond, artifactScope, labeled.EmitUnlabeledMetric),
		createSuccessCounter:     labeled.NewCounter("create_artifact_success_count", "The number of times create artifact was called", artifactScope, labeled.EmitUnlabeledMetric),
		getSuccessCounter:        labeled.NewCounter("get_artifact_success_count", "The number of times get artifact was called", artifactScope, labeled.EmitUnlabeledMetric),
		createFailureCounter:     labeled.NewCounter("create_artifact_failure_count", "The number of times create artifact failed", artifactScope, labeled.EmitUnlabeledMetric),
		getFailureCounter:        labeled.NewCounter("get_artifact_failure_count", "The number of times get artifact failed", artifactScope, labeled.EmitUnlabeledMetric),
		createDataFailureCounter: labeled.NewCounter("create_artifact_data_failure_count", "The number of times create artifact data failed", artifactScope, labeled.EmitUnlabeledMetric),
		createDataSuccessCounter: labeled.NewCounter("create_artifact_data_succeeded_count", "The number of times create artifact data succeeded", artifactScope, labeled.EmitUnlabeledMetric),
		transformerErrorCounter:  labeled.NewCounter("transformer_failed_count", "The number of times transformations failed", artifactScope, labeled.EmitUnlabeledMetric),
		validationErrorCounter:   labeled.NewCounter("validation_failed_count", "The number of times validation failed", artifactScope, labeled.EmitUnlabeledMetric),
	}

	return &artifactManager{
		repo:          repo,
		artifactStore: NewArtifactDataStore(store, storagePrefix),
		systemMetrics: artifactMetrics,
	}
}
