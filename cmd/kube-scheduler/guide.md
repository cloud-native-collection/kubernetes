# Kubernetes Scheduler 源码阅读指南

## 目录
1. [概述](#概述)
2. [源码结构](#源码结构)
3. [核心组件](#核心组件)
4. [调度流程](#调度流程)
5. [关键源码文件解析](#关键源码文件解析)
6. [扩展点和插件](#扩展点和插件)
7. [性能优化](#性能优化)
8. [调试技巧](#调试技巧)

## 概述

Kubernetes Scheduler (kube-scheduler) 是Kubernetes集群中负责Pod调度的核心组件。它的主要职责是：

- 监听未调度的Pod
- 根据调度策略和约束条件为Pod选择合适的Node
- 将调度决策写入etcd

### 版本说明
本指南基于Kubernetes 1.28+版本，源码位于 `kubernetes/pkg/scheduler/` 目录。

## 源码结构

```
pkg/scheduler/
├── apis/                    # API定义
│   └── config/             # 调度器配置API
├── framework/              # 调度框架核心
│   ├── interface.go        # 框架接口定义
│   ├── runtime/            # 运行时实现
│   └── plugins/            # 内置插件
├── profile/                # 调度配置文件
├── core/                   # 核心调度逻辑
├── metrics/                # 监控指标
└── testing/                # 测试工具
```

## 核心组件

### 1. Scheduler 主结构体

**文件位置**: `pkg/scheduler/scheduler.go`

```go
type Scheduler struct {
    SchedulerCache      internalcache.Cache
    Profiles            profile.Map
    NextPod             func() *v1.Pod
    Error               func(*v1.Pod, error)
    StopEverything      <-chan struct{}
    SchedulingQueue     internalqueue.SchedulingQueue
}
```

**关键字段说明**:
- `SchedulerCache`: 缓存集群状态信息
- `Profiles`: 调度配置文件映射
- `SchedulingQueue`: 待调度Pod队列

### 2. 调度框架 (Scheduling Framework)

**文件位置**: `pkg/scheduler/framework/interface.go`

调度框架定义了插件化的调度流程，包含以下扩展点：

```go
type Framework interface {
    QueueSortPlugin() QueueSortPlugin
    PreFilterPlugins() []PreFilterPlugin
    FilterPlugins() []FilterPlugin
    PostFilterPlugins() []PostFilterPlugin
    PreScorePlugins() []PreScorePlugin
    ScorePlugins() []ScorePlugin
    ReservePlugins() []ReservePlugin
    PermitPlugins() []PermitPlugin
    PreBindPlugins() []PreBindPlugin
    BindPlugins() []BindPlugin
    PostBindPlugins() []PostBindPlugin
}
```

## 调度流程

### 主要调度流程图

```
1. 获取待调度Pod
    ↓
2. 预选阶段 (Filtering)
    ├── PreFilter 插件
    ├── Filter 插件 (节点过滤)
    └── PostFilter 插件
    ↓
3. 优选阶段 (Scoring)
    ├── PreScore 插件
    ├── Score 插件 (节点打分)
    └── NormalizeScore
    ↓
4. 绑定阶段 (Binding)
    ├── Reserve 插件
    ├── Permit 插件
    ├── PreBind 插件
    ├── Bind 插件
    └── PostBind 插件
```

### 详细流程分析

#### 1. 调度主循环

**文件位置**: `pkg/scheduler/scheduler.go:Run()`

```go
func (sched *Scheduler) Run(ctx context.Context) {
    wait.UntilWithContext(ctx, sched.scheduleOne, 0)
}
```

#### 2. 单次调度流程

**文件位置**: `pkg/scheduler/scheduler.go:scheduleOne()`

核心步骤：
1. 从队列获取Pod
2. 执行调度算法
3. 处理调度结果
4. 绑定Pod到Node

## 关键源码文件解析

### 1. 调度算法核心

**文件**: `pkg/scheduler/core/generic_scheduler.go`

```go
func (g *genericScheduler) Schedule(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (result ScheduleResult, err error) {
    // 1. 预选阶段
    filteredNodes, filteredNodesStatuses, err := g.findNodesThatFitPod(ctx, fwk, state, pod)
    
    // 2. 优选阶段  
    priorityList, err := g.prioritizeNodes(ctx, fwk, state, pod, filteredNodes)
    
    // 3. 选择最优节点
    host, err := g.selectHost(priorityList)
    
    return ScheduleResult{SuggestedHost: host}, err
}
```

**重点方法**:
- `findNodesThatFitPod()`: 节点过滤
- `prioritizeNodes()`: 节点打分
- `selectHost()`: 选择最优节点

### 2. 节点过滤逻辑

**文件**: `pkg/scheduler/core/generic_scheduler.go:findNodesThatFitPod()`

```go
func (g *genericScheduler) findNodesThatFitPod(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) ([]*v1.Node, framework.NodeToStatusMap, error) {
    // 执行PreFilter插件
    preRes, s := fwk.RunPreFilterPlugins(ctx, state, pod)
    
    // 并行检查节点
    checkNode := func(i int) {
        // 执行Filter插件
        status := fwk.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfo)
    }
    
    // 使用工作池并行处理
    fwk.Parallelizer().Until(ctx, len(allNodes), checkNode)
}
```

### 3. 节点打分逻辑

**文件**: `pkg/scheduler/core/generic_scheduler.go:prioritizeNodes()`

```go
func (g *genericScheduler) prioritizeNodes(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod, nodes []*v1.Node) ([]framework.NodePluginScores, error) {
    // 执行PreScore插件
    preScoreStatus := fwk.RunPreScorePlugins(ctx, state, pod, nodes)
    
    // 执行Score插件
    scoresMap, scoreStatus := fwk.RunScorePlugins(ctx, state, pod, nodes)
    
    // 标准化分数
    result := make([]framework.NodePluginScores, len(nodes))
    for i := range nodes {
        result[i] = framework.NodePluginScores{
            Name:   nodes[i].Name,
            Scores: make([]framework.PluginScore, len(scoresMap)),
        }
        for j, score := range scoresMap {
            result[i].Scores[j] = framework.PluginScore{
                Name:  score.Name,
                Score: score.Scores[i].Score,
            }
        }
    }
    
    return result, nil
}
```

### 4. 缓存系统

**文件**: `pkg/scheduler/internal/cache/cache.go`

调度器缓存用于存储集群状态，包括：
- 节点信息
- Pod信息  
- 资源使用情况

```go
type cacheImpl struct {
    mu sync.RWMutex
    // 节点缓存
    nodes map[string]*nodeInfoListItem
    // Pod缓存
    pods map[string]*v1.Pod
    // 调度队列
    pendingPods map[string]*v1.Pod
}
```

### 5. 调度队列

**文件**: `pkg/scheduler/internal/queue/scheduling_queue.go`

调度队列管理待调度的Pod：

```go
type PriorityQueue struct {
    // 活跃队列 - 准备调度的Pod
    activeQ *heap.Heap
    // 退避队列 - 调度失败需要重试的Pod  
    podBackoffQ *heap.Heap
    // 不可调度队列 - 暂时无法调度的Pod
    unschedulableQ *UnschedulablePodsMap
}
```

## 扩展点和插件

### 内置插件列表

**文件位置**: `pkg/scheduler/framework/plugins/`

| 插件名 | 扩展点 | 功能描述 |
|--------|--------|----------|
| NodeResourcesFit | Filter, Score | 节点资源检查和打分 |
| NodeAffinity | Filter, Score | 节点亲和性 |
| PodTopologySpread | PreFilter, Filter | Pod拓扑分布约束 |
| TaintToleration | Filter | 污点容忍 |
| VolumeBinding | PreFilter, Filter, Reserve | 存储卷绑定 |
| ImageLocality | Score | 镜像本地性 |
| InterPodAffinity | PreFilter, Filter, Score | Pod间亲和性 |

### 自定义插件示例

```go
// 实现插件接口
type MyPlugin struct{}

func (pl *MyPlugin) Name() string {
    return "MyPlugin"
}

func (pl *MyPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
    // 自定义过滤逻辑
    return framework.NewStatus(framework.Success, "")
}

func (pl *MyPlugin) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
    // 自定义打分逻辑
    return 100, framework.NewStatus(framework.Success, "")
}
```

## 性能优化

### 1. 并行化处理

调度器使用工作池模式并行处理节点：

**文件**: `pkg/scheduler/framework/parallelize/parallelism.go`

```go
func (p *parallelizeImpl) Until(ctx context.Context, pieces int, doWorkPiece workPieceFunc) {
    // 计算worker数量
    workers := min(pieces, p.numWorkers)
    
    // 启动worker goroutines
    for i := 0; i < workers; i++ {
        go func() {
            defer wg.Done()
            for piece := range workCh {
                doWorkPiece(piece)
            }
        }()
    }
}
```

### 2. 增量更新

调度器缓存支持增量更新以提升性能：

```go
func (cache *cacheImpl) UpdatePod(oldPod, newPod *v1.Pod) error {
    // 只更新变更的字段
    if oldPod.Spec.NodeName != newPod.Spec.NodeName {
        // 处理节点变更
    }
    
    if !reflect.DeepEqual(oldPod.Spec.Containers, newPod.Spec.Containers) {
        // 处理资源变更
    }
}
```

### 3. 预选优化

使用预选扩展点减少不必要的节点检查：

```go
func (pl *NodeResourcesFitPlugin) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) *framework.Status {
    // 预计算Pod资源需求
    state.Write(preFilterStateKey, computedPodResourceRequest)
    return framework.NewStatus(framework.Success, "")
}
```

## 调试技巧

### 1. 启用调试日志

在调度器启动参数中添加：
```bash
--v=4  # 增加日志级别
--log-dir=/var/log/scheduler  # 指定日志目录
```

### 2. 使用调度器事件

查看Pod调度事件：
```bash
kubectl describe pod <pod-name>
kubectl get events --field-selector involvedObject.name=<pod-name>
```

### 3. 性能分析

启用性能分析：
```bash
--profiling=true
--bind-address=0.0.0.0
```

访问性能分析端点：
```bash
# CPU分析
curl http://scheduler-ip:10251/debug/pprof/profile?seconds=30

# 内存分析  
curl http://scheduler-ip:10251/debug/pprof/heap
```

### 4. 常见问题排查

#### Pod一直Pending
1. 检查节点资源是否充足
2. 验证Pod的资源请求
3. 检查节点亲和性和反亲和性规则
4. 验证污点和容忍设置

#### 调度性能差
1. 检查集群节点数量
2. 分析调度器日志中的耗时
3. 调整调度器并发参数
4. 考虑使用调度器性能调优参数

### 5. 源码调试环境搭建

```bash
# 1. 下载源码
git clone https://github.com/kubernetes/kubernetes.git
cd kubernetes

# 2. 构建调度器
make WHAT=cmd/kube-scheduler

# 3. 本地运行调度器
./_output/bin/kube-scheduler \
    --config=/path/to/scheduler-config.yaml \
    --v=4
```

### 6. 单元测试

运行调度器相关测试：
```bash
# 运行所有调度器测试
make test WHAT=./pkg/scheduler/...

# 运行特定测试
go test -v ./pkg/scheduler/core/... -run TestGenericScheduler
```

## 总结

通过这个源码阅读指南，你应该能够：

1. 理解kube-scheduler的整体架构
2. 掌握核心调度流程和算法
3. 了解如何扩展和自定义调度器
4. 具备调试和性能优化能力

建议按照以下顺序深入学习：

1. 先理解整体架构和调度流程
2. 深入研究核心算法实现
3. 学习插件机制和扩展方法
4. 实践性能优化和问题排查

