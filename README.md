# go-forest-agent

基于 [go-forest v0.3.0](https://github.com/Leeminjing/go-forest/releases/tag/v0.3.0) 的独立 Agent Runtime 扩展，Go 1.23+，只使用 Go 标准库。

Agent、Model、Tool、Checkpoint、Event、Approval 都是普通 Tree，通过声明的 Port 和本地 Link 组合。Forest Core 无 Agent 专属逻辑。本阶段不接入或替换 Focus，不创建 Focus-Go 产品。

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

## 检查点和工具副作用

每次工具执行前先保存 tool-running 状态、调用 ID、参数与工具版本。已保存结果不会重放。执行异常、panic、取消或结果保存失败都可能意味着副作用已经发生，Run 会进入 outcome-unknown，或在恢复时从 tool-running 转为该状态。

宿主确认旧执行者已经停止后，可以在新 Scope 中调用 Recover。模型执行中的中断回到 model-ready；工具执行中的中断进入 outcome-unknown。初版没有跨进程租约，也不会自动判定旧执行者已死亡。禁止对仍活动的执行者调用管理恢复操作。

unknown 状态必须由宿主核实外部结果，再通过 Resolve 提供已知结果。Resolve 不重新执行工具；没有“盲目重试工具”的便捷入口。Decide 和 Resolve 是受信宿主的管理 API，接入 HTTP/产品时必须由宿主做身份认证和授权。

审批绑定当前调用 ID、参数及完整 ToolSpec。工具版本或说明改变会清除旧批准并重新检查。工具参数的业务校验由 Tool 实现；模型只能请求已连接的工具。任何工具 error 都保守地视为结果未知，包括可能尚未产生副作用的参数错误。

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
