# go-forest-agent

基于 [go-forest v0.3.0](https://github.com/Leeminjing/go-forest/releases/tag/v0.3.0) 的独立 Agent Runtime 扩展，Go 1.23+，只使用 Go 标准库。

Agent、Model、Tool、Checkpoint、Event、Approval 都是普通 Tree，通过声明的 Port 和本地 Link 组合。Forest Core 无 Agent 专属逻辑。本阶段不接入或替换 Focus，不创建 Focus-Go 产品。

在外部 Go 模块中使用固定版本：`go get github.com/Leeminjing/go-forest-agent/agent@v0.2.0`。已有 v0.1.0 标签保持不变。

## 可选 Responses 模型树

`trees/openairesponses` 实现原有消费方 `agent.Model` 接口，`trees/responsejournal` 实现适配器声明的 Journal 契约。HTTP/SSE、模型配置、工具调用和原生续接都在可选树内，Agent 状态机与 Forest Core 不含供应商分支。

```go
config := openairesponses.Config{
    APIKey: os.Getenv("OPENAI_API_KEY"),
    Model: os.Getenv("OPENAI_MODEL"),
    Instructions: "Use the connected tools; report only verified results.",
}
// Register both ordinary Definitions and connect model.journal to journal.journal.
modelDefinition := openairesponses.Definition(config)
journalDefinition := responsejournal.Definition("data/responses")
```

默认请求 `https://api.openai.com/v1/responses`；Endpoint 只由可信宿主配置，也可以指定确实支持所需 Responses 协议的兼容端点。没有 Chat Completions 回退。请求使用自定义 function tools、stream、store=false 和禁用隐式截断；不安装供应商内置 Shell/MCP/网络工具，不自动重试 POST。供应商可能不支持所有请求参数，必须按实际端点验收。本次联网验证使用用户明确指定的 DeepSeek Responses，不能视为 OpenAI 托管服务验证。

临时文本通过现有 emit 回调发送，只有完整响应才返回可执行的工具计划。拒绝、未完成、断流、未知输出或只有 commentary 的响应不会伪装成最终回答；Context/回调错误会终止流。取消 HTTP 不承诺远端没有产生费用。

Journal 在返回模型结果前保存完整原生输出，包括 reasoning 和 phase；当前 Run 后续请求重放这些项并追加工具结果。标准 Agent 消息只是消费视图。相同请求 fingerprint 已保存时直接返回原结果；必需记录缺失、损坏或消息前缀不匹配时失败，不丢弃续接数据后重发。兼容端点的明文 reasoning 仅作为原生记录保存，不通过文本回调或业务回答暴露。

Journal 使用不可覆盖序号文件、SHA256 和原子硬链接，需支持硬链接的本地文件系统；不覆盖损坏记录或回退旧记录。原生记录最多 8 MiB，单 Run 链最多 32 MiB；不承诺断电耐久性或 API exactly-once。API key 不进入 profile、记录、事件和错误正文。Query/归档可使用 Journal.Load 获取脱离存储的完整副本。

## 运行示例

```sh
go run ./examples/agent-loop -directory .scratch/demo -id example
go run ./examples/agent-loop -directory .scratch/demo -id example -approve -model-version 2
```

第一次执行模型计划后暂停，输出 `status=awaiting-approval`，没有调用工具。第二个进程载入同一检查点，显式批准该调用后执行加法工具，流式输出临时 delta，保存最终回答 `model 2: 5`。再次执行同一 ID 直接读取已完成状态，不重复执行工具。

示例使用确定性 Model，专门验证实际执行循环及可替换契约，不调用线上语言模型，也不需要密钥。供应商可以显式实现消费方的 Model 接口，作为自己的普通 Definition 注册；初版不包含供应商 SDK。

## 调用及 Scope

应用先通过 Forest.Invoke 和 forest.Use[agent.Executor] 创建 Run：

```go
err := f.Invoke(ctx, func(stepCtx context.Context) error {
    return forest.Use[agent.Executor](stepCtx, f, "agent", "executor",
        func(executor agent.Executor) error {
            _, err := executor.Create(stepCtx, "run-1", "Calculate 2 + 3")
            return err
        })
})
// ctx 必须来自森林 Scope 之外。
run, err := agent.Continue(ctx, f, "agent", "run-1")
```

Continue 在每个 Advance 建立并释放独立 Scope。当前步骤使用固定版本；模型替换后的后续步骤可以使用新版本。暂停和重启都不持有旧 Scope，不将 Generation 当作可跨重启恢复的执行上下文。在已有 Scope 中调用 Continue 返回 ErrNestedScope。

Runtime 需要 model、checkpoint、events、approval 四个必需端口；tools 为可选多值端口。Checkpoint 是构造期资源依赖，不能在保留 Runtime 时悄悄换成另一个存储。示例展示完整注册和 ChangePlan 接线。

Tool.Describe 与 Approval.Check 是准入前查询，提供者不应在这些方法中执行工具业务副作用。模型和工具执行的准入分别由 Generate/Execute 之前的持久状态保护；事件使用持久记录与幂等投递。

## 检查点和工具副作用

每次工具执行前先保存 tool-running 状态、调用 ID、参数与工具版本。已保存结果不会重放。执行异常、panic、取消或结果保存失败都可能意味着副作用已经发生，Run 会进入 outcome-unknown，或在恢复时从 tool-running 转为该状态。

宿主确认旧执行者已经停止后，可以在新 Scope 中调用 Recover。模型执行中的中断回到 model-ready；工具执行中的中断进入 outcome-unknown。初版没有跨进程租约，也不会自动判定旧执行者已死亡。禁止对仍活动的执行者调用管理恢复操作。

unknown 状态必须由宿主核实外部结果，再通过 Resolve 提供已知结果。Resolve 不重新执行工具；没有“盲目重试工具”的便捷入口。Decide 和 Resolve 是受信宿主的管理 API，接入 HTTP/产品时必须由宿主做身份认证和授权。

审批绑定当前调用 ID、参数及完整 ToolSpec。Decide 必须携带操作者观察到的 Revision，过时返回 ErrConflict；工具版本或说明改变会清除旧批准并重新检查。工具参数的业务校验由 Tool 实现；模型只能请求已连接的工具。任何工具 error 都保守地视为结果未知，包括可能尚未产生副作用的参数错误。

文件检查点按 Run ID 的 SHA256 分目录，追加不可覆盖的 revision 文件：写入临时文件、fsync，再以原子硬链接发布。需要支持硬链接的本地文件系统；不支持时返回错误，无非原子覆盖降级。最高 revision 损坏会返回 ErrCorrupt，不回退旧状态。Windows 验证进程终止恢复，不承诺断电持久性；不提供分布式事务、加密存储或不可信本地用户的沙箱。

## 事件和并发

最终消息、工具结果和事件在同一个检查点保存；事件以 RunID+Sequence 标识，先发布再保存确认游标，因此崩溃窗口允许重复投递。Events 实现必须幂等。trees/events 提供有容量限制的内存接收者，订阅落后报告 ErrGap，此时重读检查点；内存事件不跨进程持久化。

model.delta 是临时展示输出，可能中断、重复，不能当作已提交回答。模型返回后再调用旧 emit 回调会被拒绝。最终结果以检查点为准。

不同 Run 独立推进；同一 Run 通过 Checkpoint CAS 竞争步骤准入，活动步骤返回 ErrBusy。Context 取消会传到提供者，已知结果有默认 5 秒的独立提交期限。忽略 Context 的任意 goroutine 无法由运行时强制终止。

默认模型调用上限 32，可配置到 128；每次模型最多 16 个工具调用，最多 64 个已连接工具。临时 delta 最多 4096 段、总计 4 MiB；持久事件最多 16384 条，文件记录最多 32 MiB。超限返回明确错误。

## 验证

```sh
go vet ./...
go test ./... -timeout 90s
go test -race ./... -count=3 -timeout 120s
```

真实子进程测试在工具执行前、产生副作用后及结果保存后真正终止进程，再用新进程恢复，检查不会重复执行未知或已保存的工具。另覆盖热替换、审批、取消、并发隔离、CAS 冲突、事件重复投递和存储损坏。

Spec 和 Design 仅保存在开发者本地，由 .gitignore 排除，不进入 Git 历史。公开验收与审查记录在 docs/。
