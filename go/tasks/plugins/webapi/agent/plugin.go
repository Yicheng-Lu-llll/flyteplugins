package agent

import (
	"context"
	"encoding/gob"
	"fmt"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/admin"

	"google.golang.org/grpc/grpclog"

	flyteIdl "github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/service"
	pluginErrors "github.com/flyteorg/flyteplugins/go/tasks/errors"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/core"
	pluginsCore "github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/core"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/ioutils"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/webapi"
	"github.com/flyteorg/flytestdlib/promutils"
	"google.golang.org/grpc"
)

type GetClientFunc func(ctx context.Context, endpoint string, connectionCache map[string]*grpc.ClientConn) (service.AsyncAgentServiceClient, error)

type Plugin struct {
	metricScope     promutils.Scope
	cfg             *Config
	getClient       GetClientFunc
	connectionCache map[string]*grpc.ClientConn
}

type ResourceWrapper struct {
	State   admin.State
	Outputs *flyteIdl.LiteralMap
}

type ResourceMetaWrapper struct {
	OutputPrefix      string
	Token             string
	AgentResourceMeta []byte
	TaskType          string
}

func (p Plugin) GetConfig() webapi.PluginConfig {
	return GetConfig().WebAPI
}

func (p Plugin) ResourceRequirements(_ context.Context, _ webapi.TaskExecutionContextReader) (
	namespace core.ResourceNamespace, constraints core.ResourceConstraintsSpec, err error) {

	// Resource requirements are assumed to be the same.
	return "default", p.cfg.ResourceConstraints, nil
}

func (p Plugin) Create(ctx context.Context, taskCtx webapi.TaskExecutionContextReader) (webapi.ResourceMeta,
	webapi.Resource, error) {
	taskTemplate, err := taskCtx.TaskReader().Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	inputs, err := taskCtx.InputReader().Get(ctx)
	if err != nil {
		return nil, nil, err
	}

	outputPrefix := taskCtx.OutputWriter().GetOutputPrefixPath().String()

	endpoint := getFinalEndpoint(taskTemplate.Type, p.cfg.DefaultGrpcEndpoint, p.cfg.EndpointForTaskTypes)
	client, err := p.getClient(ctx, endpoint, p.connectionCache)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to agent with error: %v", err)
	}

	res, err := client.CreateTask(ctx, &admin.CreateTaskRequest{Inputs: inputs, Template: taskTemplate, OutputPrefix: outputPrefix})
	if err != nil {
		return nil, nil, err
	}

	return &ResourceMetaWrapper{
		OutputPrefix:      outputPrefix,
		AgentResourceMeta: res.GetResourceMeta(),
		Token:             "",
		TaskType:          taskTemplate.Type,
	}, &ResourceWrapper{State: admin.State_RUNNING}, nil
}

func (p Plugin) Get(ctx context.Context, taskCtx webapi.GetContext) (latest webapi.Resource, err error) {
	metadata := taskCtx.ResourceMeta().(*ResourceMetaWrapper)

	endpoint := getFinalEndpoint(metadata.TaskType, p.cfg.DefaultGrpcEndpoint, p.cfg.EndpointForTaskTypes)
	client, err := p.getClient(ctx, endpoint, p.connectionCache)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent with error: %v", err)
	}

	res, err := client.GetTask(ctx, &admin.GetTaskRequest{TaskType: metadata.TaskType, ResourceMeta: metadata.AgentResourceMeta})
	if err != nil {
		return nil, err
	}

	return &ResourceWrapper{
		State:   res.Resource.State,
		Outputs: res.Resource.Outputs,
	}, nil
}

func (p Plugin) Delete(ctx context.Context, taskCtx webapi.DeleteContext) error {
	if taskCtx.ResourceMeta() == nil {
		return nil
	}
	metadata := taskCtx.ResourceMeta().(ResourceMetaWrapper)

	endpoint := getFinalEndpoint(metadata.TaskType, p.cfg.DefaultGrpcEndpoint, p.cfg.EndpointForTaskTypes)
	client, err := p.getClient(ctx, endpoint, p.connectionCache)
	if err != nil {
		return fmt.Errorf("failed to connect to agent with error: %v", err)
	}

	_, err = client.DeleteTask(ctx, &admin.DeleteTaskRequest{TaskType: metadata.TaskType, ResourceMeta: metadata.AgentResourceMeta})
	return err
}

func (p Plugin) Status(ctx context.Context, taskCtx webapi.StatusContext) (phase core.PhaseInfo, err error) {
	resource := taskCtx.Resource().(*ResourceWrapper)
	taskInfo := &core.TaskInfo{}

	switch resource.State {
	case admin.State_RUNNING:
		return core.PhaseInfoRunning(pluginsCore.DefaultPhaseVersion, taskInfo), nil
	case admin.State_PERMANENT_FAILURE:
		return core.PhaseInfoFailure(pluginErrors.TaskFailedWithError, "failed to run the job", taskInfo), nil
	case admin.State_RETRYABLE_FAILURE:
		return core.PhaseInfoRetryableFailure(pluginErrors.TaskFailedWithError, "failed to run the job", taskInfo), nil
	case admin.State_SUCCEEDED:
		if resource.Outputs != nil {
			err := taskCtx.OutputWriter().Put(ctx, ioutils.NewInMemoryOutputReader(resource.Outputs, nil, nil))
			if err != nil {
				return core.PhaseInfoUndefined, err
			}
		}
		return core.PhaseInfoSuccess(taskInfo), nil
	}
	return core.PhaseInfoUndefined, pluginErrors.Errorf(pluginsCore.SystemErrorCode, "unknown execution phase [%v].", resource.State)
}

func getFinalEndpoint(taskType, defaultEndpoint string, endpointForTaskTypes map[string]string) string {
	if t, exists := endpointForTaskTypes[taskType]; exists {
		return t
	}

	return defaultEndpoint
}

func getClientFunc(ctx context.Context, endpoint string, connectionCache map[string]*grpc.ClientConn) (service.AsyncAgentServiceClient, error) {
	conn, ok := connectionCache[endpoint]
	if ok {
		return service.NewAsyncAgentServiceClient(conn), nil
	}
	var opts []grpc.DialOption
	var err error

	opts = append(opts, grpc.WithInsecure())
	conn, err = grpc.Dial(endpoint, opts...)
	if err != nil {
		return nil, err
	}
	connectionCache[endpoint] = conn
	defer func() {
		if err != nil {
			if cerr := conn.Close(); cerr != nil {
				grpclog.Infof("Failed to close conn to %s: %v", endpoint, cerr)
			}
			return
		}
		go func() {
			<-ctx.Done()
			if cerr := conn.Close(); cerr != nil {
				grpclog.Infof("Failed to close conn to %s: %v", endpoint, cerr)
			}
		}()
	}()
	return service.NewAsyncAgentServiceClient(conn), nil
}

func newAgentPlugin() webapi.PluginEntry {
	supportedTaskTypes := GetConfig().SupportedTaskTypes

	return webapi.PluginEntry{
		ID:                 "agent-service",
		SupportedTaskTypes: supportedTaskTypes,
		PluginLoader: func(ctx context.Context, iCtx webapi.PluginSetupContext) (webapi.AsyncPlugin, error) {
			return &Plugin{
				metricScope:     iCtx.MetricsScope(),
				cfg:             GetConfig(),
				getClient:       getClientFunc,
				connectionCache: make(map[string]*grpc.ClientConn),
			}, nil
		},
	}
}

func RegisterAgentPlugin() {
	gob.Register(ResourceMetaWrapper{})
	gob.Register(ResourceWrapper{})

	pluginmachinery.PluginRegistry().RegisterRemotePlugin(newAgentPlugin())
}
