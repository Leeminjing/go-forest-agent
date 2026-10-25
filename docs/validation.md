# Agent Runtime 初版验收

日期：2026-10-25。依赖正式发布的 go-forest v0.3.0，无本地 replace，无 Forest Core 改动。Spec、Design 仅本地保存，未进入 Git 历史。

## 行为证据

| 行为 | 自动化证据 |
| --- | --- |
| Model → Tool → Model，结果保存后不重放 | TestModelToolModelDurableEventsAndNoReplay：完整循环完成；重新组合森林读取同一检查点，工具计数仍为 1。 |
| 每步版本边界与模型热替换 | TestEachStepSeesNewModelAndOldScopeDrains：阻塞旧模型，替换后旧实例仍未 Stop；释放旧调用后，后续模型步骤使用新版本；旧资源最后退出。已有 Scope 中驱动 Continue 被拒绝。 |
| 审批暂停、拒绝与恢复 | TestApprovalRestartDenialAndChangedToolIdentity：新森林载入暂停状态，批准后继续；工具变化使旧批准失效；过时 Revision 不能批准新的暂停；拒绝后工具计数不变。 |
| 工具已执行但结果保存失败 | TestToolResultWriteFailureRequiresExplicitResolution：注入结果写入失败，持久记录仍是 tool-running；Recover 进入 unknown，不重放；显式 Resolve 后继续。 |
| 取消 | TestCancellationPropagatesAndToolUncertaintyIsPreserved：模型和工具都收到取消；模型取消被保存，工具取消保留 unknown。 |
| 并发隔离及准入 | TestConcurrentRunIsolationAndSingleRunClaims：同一 Run 的活动步骤返回 busy；12 个独立 Run 并发完成，工具计数无串扰。 |
| 真实进程崩溃 | TestRealProcessCrashPreservesUnknownAndSavedToolResults：实际 Kill 三个阶段的子进程，再以全新进程打开同目录恢复。未执行工具正常继续；副作用已发生且未保存结果时保持 unknown；已保存结果不重复执行。每种情形最终副作用文件只有一条记录。 |
| 调用及输出边界 | TestInvalidModelAndUnconnectedToolsFailWithoutPanic、TestHotReplacedInvalidToolDescriptionIsAnError：非法 JSON、重复模型调用 ID、未连接工具、无效升级工具说明明确报错，无误执行。 |
| 流式输出及限制 | TestNoToolsAndConcurrentStreamingCallbacks、TestModelTurnLimitAndUnknownOnToolPanic：无工具模型正常工作，并发 delta 安全；过期 emit 被拒绝；模型次数有界；工具 panic 进入 unknown。 |
| 事件投递 | TestDurableEventsReplayAfterAcknowledgementFailure：事件发送成功但确认保存失败，随后重投仍由 Event Tree 去重。TestDurableDeduplicationBoundAndSubscriberGap 验证容量与订阅缺口。 |
| 文件 CAS 与损坏 | TestAtomicCASAcrossStoreInstancesAndCorruptLatest：16 个 Store 竞争只有一个成功；最高 revision 损坏不回退。TestDetachedDataAndCancellation 验证副本及取消。 |
| 语义损坏及 UTF-8 | TestInvalidUTF8AndSemanticStateAreNotNormalizedOrResumed：重复调用身份、丢失整个计划、计数矛盾、非法源字符串和损坏字节均拒绝，不默默改变数据后执行。 |

## 独立演示

两个独立命令先后运行：

```sh
go run ./examples/agent-loop -directory .scratch/demo-checkpoints
go run ./examples/agent-loop -directory .scratch/demo-checkpoints -approve -model-version 2
```

首个命令暂停于 awaiting-approval；第二个载入检查点并显式批准后，输出 delta 和最终结果 `model 2: 5`，状态 completed。示例使用确定性模型，不宣称调用线上语言模型。

## 工程验证

```sh
go vet ./...
go test ./... -timeout 90s
go test -race ./... -count=3 -timeout 120s
```

Windows 完整测试及 vet 通过。Linux Go 1.23 使用 Docker 执行 race；子进程复用带 race 检测的测试二进制，故意终止时也检查 stderr 中已有的 race 报告。最终执行结果在独立审查修复后记录。

最终三轮 race 全部通过：执行器集成测试 14.968 秒，事件树 1.018 秒，文件检查点 1.157 秒，无竞态报告。本地 Docker 使用只读模块缓存取得已下载的 v0.3.0 依赖，未使用本地 replace 或修改依赖源码。

CI 同时在 Go 1.23 Windows/Linux 执行 vet、测试与双进程示例；Linux 执行三轮 race。

实现提交 e0e73ff 的 [GitHub Actions](https://github.com/Leeminjing/go-forest-agent/actions/runs/40163698612) 全部通过。独立 Go 模块实际从远程下载 `github.com/Leeminjing/go-forest-agent@v0.0.0-20261025045553-e0e73ff021aa`，同时下载 go-forest v0.3.0，导入执行器及 scriptedmodel 包并运行成功，未使用 replace。

## 边界

本阶段只完成独立扩展；不接入 Focus Context、不修改 Python Focus、不创建 Focus-Go。没有供应商 API、分布式租约、自动工具重放或 exactly-once 声明。管理恢复要求宿主已确认旧执行者停止。运行中的普通业务仍依赖 Context 协作取消。审查记录见 review.md。
